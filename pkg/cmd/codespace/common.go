package codespace

// This file defines functions common to the entire codespace command set.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"github.com/cli/cli/v2/internal/codespaces"
	"github.com/cli/cli/v2/internal/codespaces/api"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/cli/cli/v2/pkg/liveshare"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type browser interface {
	Browse(string) error
}

type executable interface {
	Executable() string
}

type App struct {
	io         *iostreams.IOStreams
	apiClient  apiClient
	errLogger  *log.Logger
	executable executable
	browser    browser
}

func NewApp(io *iostreams.IOStreams, exe executable, apiClient apiClient, browser browser) *App {
	errLogger := log.New(io.ErrOut, "", 0)

	return &App{
		io:         io,
		apiClient:  apiClient,
		errLogger:  errLogger,
		executable: exe,
		browser:    browser,
	}
}

// StartProgressIndicatorWithLabel starts a progress indicator with a message.
func (a *App) StartProgressIndicatorWithLabel(s string) {
	a.io.StartProgressIndicatorWithLabel(s)
}

// StopProgressIndicator stops the progress indicator.
func (a *App) StopProgressIndicator() {
	a.io.StopProgressIndicator()
}

// Connects to a codespace using Live Share and returns that session
func startLiveShareSession(ctx context.Context, codespace *api.Codespace, a *App, debug bool, debugFile string) (session *liveshare.Session, err error) {
	// While connecting, ensure in the background that the user has keys installed.
	// That lets us report a more useful error message if they don't.
	authkeys := make(chan error, 1)
	go func() {
		authkeys <- checkAuthorizedKeys(ctx, a.apiClient)
	}()

	liveshareLogger := noopLogger()
	if debug {
		debugLogger, err := newFileLogger(debugFile)
		if err != nil {
			return nil, fmt.Errorf("couldn't create file logger: %w", err)
		}
		defer safeClose(debugLogger, &err)

		liveshareLogger = debugLogger.Logger
		a.errLogger.Printf("Debug file located at: %s", debugLogger.Name())
	}

	session, err = codespaces.ConnectToLiveshare(ctx, a, liveshareLogger, a.apiClient, codespace)
	if err != nil {
		if authErr := <-authkeys; authErr != nil {
			return nil, fmt.Errorf("failed to fetch authorization keys: %w", authErr)
		}
		return nil, fmt.Errorf("failed to connect to Live Share: %w", err)
	}
	return
}

//go:generate moq -fmt goimports -rm -skip-ensure -out mock_api.go . apiClient
type apiClient interface {
	GetUser(ctx context.Context) (*api.User, error)
	GetCodespace(ctx context.Context, name string, includeConnection bool) (*api.Codespace, error)
	ListCodespaces(ctx context.Context, limit int) ([]*api.Codespace, error)
	DeleteCodespace(ctx context.Context, name string) error
	StartCodespace(ctx context.Context, name string) error
	StopCodespace(ctx context.Context, name string) error
	CreateCodespace(ctx context.Context, params *api.CreateCodespaceParams) (*api.Codespace, error)
	EditCodespace(ctx context.Context, codespaceName string, params *api.EditCodespaceParams) (*api.Codespace, error)
	GetRepository(ctx context.Context, nwo string) (*api.Repository, error)
	AuthorizedKeys(ctx context.Context, user string) ([]byte, error)
	GetCodespacesMachines(ctx context.Context, repoID int, branch, location string) ([]*api.Machine, error)
	GetCodespaceRepositoryContents(ctx context.Context, codespace *api.Codespace, path string) ([]byte, error)
	ListDevContainers(ctx context.Context, repoID int, branch string, limit int) (devcontainers []api.DevContainerEntry, err error)
	GetCodespaceRepoSuggestions(ctx context.Context, partialSearch string, params api.RepoSearchParameters) ([]string, error)
}

var errNoCodespaces = errors.New("you have no codespaces")

func chooseCodespace(ctx context.Context, apiClient apiClient) (*api.Codespace, error) {
	codespaces, err := apiClient.ListCodespaces(ctx, -1)
	if err != nil {
		return nil, fmt.Errorf("error getting codespaces: %w", err)
	}
	return chooseCodespaceFromList(ctx, codespaces)
}

// chooseCodespaceFromList returns the selected codespace from the list,
// or an error if there are no codespaces.
func chooseCodespaceFromList(ctx context.Context, codespaces []*api.Codespace) (*api.Codespace, error) {
	if len(codespaces) == 0 {
		return nil, errNoCodespaces
	}

	sort.Slice(codespaces, func(i, j int) bool {
		return codespaces[i].CreatedAt > codespaces[j].CreatedAt
	})

	type codespaceWithIndex struct {
		cs  codespace
		idx int
	}

	namesWithConflict := make(map[string]bool)
	codespacesByName := make(map[string]codespaceWithIndex)
	codespacesNames := make([]string, 0, len(codespaces))
	for _, apiCodespace := range codespaces {
		cs := codespace{apiCodespace}
		csName := cs.displayName(false, false)
		displayNameWithGitStatus := cs.displayName(false, true)

		_, hasExistingConflict := namesWithConflict[csName]
		if seenCodespace, ok := codespacesByName[csName]; ok || hasExistingConflict {
			// There is an existing codespace on the repo and branch.
			// We need to disambiguate by adding the codespace name
			// to the existing entry and the one we are processing now.
			if !hasExistingConflict {
				fullDisplayName := seenCodespace.cs.displayName(true, false)
				fullDisplayNameWithGitStatus := seenCodespace.cs.displayName(true, true)

				codespacesByName[fullDisplayName] = codespaceWithIndex{seenCodespace.cs, seenCodespace.idx}
				codespacesNames[seenCodespace.idx] = fullDisplayNameWithGitStatus
				delete(codespacesByName, csName) // delete the existing map entry with old name

				// All other codespaces with the same name should update
				// to their specific name, this tracks conflicting names going forward
				namesWithConflict[csName] = true
			}

			// update this codespace names to include the name to disambiguate
			csName = cs.displayName(true, false)
			displayNameWithGitStatus = cs.displayName(true, true)
		}

		codespacesByName[csName] = codespaceWithIndex{cs, len(codespacesNames)}
		codespacesNames = append(codespacesNames, displayNameWithGitStatus)
	}

	csSurvey := []*survey.Question{
		{
			Name: "codespace",
			Prompt: &survey.Select{
				Message: "Choose codespace:",
				Options: codespacesNames,
				Default: codespacesNames[0],
			},
			Validate: survey.Required,
		},
	}

	var answers struct {
		Codespace string
	}
	if err := ask(csSurvey, &answers); err != nil {
		return nil, fmt.Errorf("error getting answers: %w", err)
	}

	// Codespaces are indexed without the git status included as compared
	// to how it is displayed in the prompt, so the git status symbol needs
	// cleaning up in case it is included.
	selectedCodespace := strings.Replace(answers.Codespace, gitStatusDirty, "", -1)
	return codespacesByName[selectedCodespace].cs.Codespace, nil
}

// getOrChooseCodespace prompts the user to choose a codespace if the codespaceName is empty.
// It then fetches the codespace record with full connection details.
// TODO(josebalius): accept a progress indicator or *App and show progress when fetching.
func getOrChooseCodespace(ctx context.Context, apiClient apiClient, codespaceName string) (codespace *api.Codespace, err error) {
	if codespaceName == "" {
		codespace, err = chooseCodespace(ctx, apiClient)
		if err != nil {
			if err == errNoCodespaces {
				return nil, err
			}
			return nil, fmt.Errorf("choosing codespace: %w", err)
		}
	} else {
		codespace, err = apiClient.GetCodespace(ctx, codespaceName, true)
		if err != nil {
			return nil, fmt.Errorf("getting full codespace details: %w", err)
		}
	}

	if codespace.PendingOperation {
		return nil, fmt.Errorf(
			"codespace is disabled while it has a pending operation: %s",
			codespace.PendingOperationDisabledReason,
		)
	}

	return codespace, nil
}

func safeClose(closer io.Closer, err *error) {
	if closeErr := closer.Close(); *err == nil {
		*err = closeErr
	}
}

// hasTTY indicates whether the process connected to a terminal.
// It is not portable to assume stdin/stdout are fds 0 and 1.
var hasTTY = term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))

// ask asks survey questions on the terminal, using standard options.
// It fails unless hasTTY, but ideally callers should avoid calling it in that case.
func ask(qs []*survey.Question, response interface{}) error {
	if !hasTTY {
		return fmt.Errorf("no terminal")
	}
	err := survey.Ask(qs, response, survey.WithShowCursor(true))
	// The survey package temporarily clears the terminal's ISIG mode bit
	// (see tcsetattr(3)) so the QUIT button (Ctrl-C) is reported as
	// ASCII \x03 (ETX) instead of delivering SIGINT to the application.
	// So we have to serve ourselves the SIGINT.
	//
	// https://github.com/AlecAivazis/survey/#why-isnt-ctrl-c-working
	if err == terminal.InterruptErr {
		self, _ := os.FindProcess(os.Getpid())
		_ = self.Signal(os.Interrupt) // assumes POSIX

		// Suspend the goroutine, to avoid a race between
		// return from main and async delivery of INT signal.
		select {}
	}
	return err
}

// checkAuthorizedKeys reports an error if the user has not registered any SSH keys;
// see https://github.com/cli/cli/v2/issues/166#issuecomment-921769703.
// The check is not required for security but it improves the error message.
func checkAuthorizedKeys(ctx context.Context, client apiClient) error {
	user, err := client.GetUser(ctx)
	if err != nil {
		return fmt.Errorf("error getting user: %w", err)
	}

	keys, err := client.AuthorizedKeys(ctx, user.Login)
	if err != nil {
		return fmt.Errorf("failed to read GitHub-authorized SSH keys for %s: %w", user, err)
	}
	if len(keys) == 0 {
		return fmt.Errorf("user %s has no GitHub-authorized SSH keys", user)
	}
	return nil // success
}

var ErrTooManyArgs = errors.New("the command accepts no arguments")

func noArgsConstraint(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return ErrTooManyArgs
	}
	return nil
}

func noopLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

type codespace struct {
	*api.Codespace
}

// displayName returns the repository nwo and branch.
// If includeName is true, the name of the codespace (including displayName) is included.
// If includeGitStatus is true, the branch will include a star if
// the codespace has unsaved changes.
func (c codespace) displayName(includeName, includeGitStatus bool) string {
	branch := c.GitStatus.Ref
	if includeGitStatus {
		branch = c.branchWithGitStatus()
	}

	if includeName {
		var displayName = c.Name
		if c.DisplayName != "" {
			displayName = c.DisplayName
		}
		return fmt.Sprintf(
			"%s: %s (%s)", c.Repository.FullName, displayName, branch,
		)
	}
	return fmt.Sprintf(
		"%s: %s", c.Repository.FullName, branch,
	)

}

// gitStatusDirty represents an unsaved changes status.
const gitStatusDirty = "*"

// branchWithGitStatus returns the branch with a star
// if the branch is currently being worked on.
func (c codespace) branchWithGitStatus() string {
	if c.hasUnsavedChanges() {
		return c.GitStatus.Ref + gitStatusDirty
	}

	return c.GitStatus.Ref
}

// hasUnsavedChanges returns whether the environment has
// unsaved changes.
func (c codespace) hasUnsavedChanges() bool {
	return c.GitStatus.HasUncommitedChanges || c.GitStatus.HasUnpushedChanges
}

// running returns whether the codespace environment is running.
func (c codespace) running() bool {
	return c.State == api.CodespaceStateAvailable
}
