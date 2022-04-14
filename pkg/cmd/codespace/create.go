package codespace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/cli/cli/v2/internal/codespaces"
	"github.com/cli/cli/v2/internal/codespaces/api"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/utils"
	"github.com/spf13/cobra"
)

type createOptions struct {
	repo              string
	branch            string
	location          string
	machine           string
	showStatus        bool
	permissionsOptOut bool
	idleTimeout       time.Duration
}

func newCreateCmd(app *App) *cobra.Command {
	opts := createOptions{}

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a codespace",
		Args:  noArgsConstraint,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Create(cmd.Context(), opts)
		},
	}

	createCmd.Flags().StringVarP(&opts.repo, "repo", "r", "", "repository name with owner: user/repo")
	createCmd.Flags().StringVarP(&opts.branch, "branch", "b", "", "repository branch")
	createCmd.Flags().StringVarP(&opts.location, "location", "l", "", "location: {EastUs|SouthEastAsia|WestEurope|WestUs2} (determined automatically if not provided)")
	createCmd.Flags().StringVarP(&opts.machine, "machine", "m", "", "hardware specifications for the VM")
	createCmd.Flags().BoolVarP(&opts.permissionsOptOut, "default-permissions", "", false, "do not prompt to accept additional permissions requested by the codespace")
	createCmd.Flags().BoolVarP(&opts.showStatus, "status", "s", false, "show status of post-create command and dotfiles")
	createCmd.Flags().DurationVar(&opts.idleTimeout, "idle-timeout", 0, "allowed inactivity before codespace is stopped, e.g. \"10m\", \"1h\"")

	return createCmd
}

// Create creates a new Codespace
func (a *App) Create(ctx context.Context, opts createOptions) error {
	// Overrides for Codespace developers to target test environments
	vscsLocation := os.Getenv("VSCS_LOCATION")
	vscsTarget := os.Getenv("VSCS_TARGET")
	vscsTargetUrl := os.Getenv("VSCS_TARGET_URL")

	userInputs := struct {
		Repository string
		Branch     string
		Location   string
	}{
		Repository: opts.repo,
		Branch:     opts.branch,
		Location:   opts.location,
	}

	if userInputs.Repository == "" {
		branchPrompt := "Branch (leave blank for default branch):"
		if userInputs.Branch != "" {
			branchPrompt = "Branch:"
		}
		questions := []*survey.Question{
			{
				Name: "repository",
				Prompt: &survey.Input{
					Message: "Repository:",
					Help:    "Search for repos by name. To search within an org or user, or to see private repos, enter at least ':user/'.",
					Suggest: func(toComplete string) []string {
						return getRepoSuggestions(ctx, a.apiClient, toComplete)
					},
				},
				Validate: survey.Required,
			},
			{
				Name: "branch",
				Prompt: &survey.Input{
					Message: branchPrompt,
					Default: userInputs.Branch,
				},
			},
		}
		if err := ask(questions, &userInputs); err != nil {
			return fmt.Errorf("failed to prompt: %w", err)
		}
	}

	if userInputs.Location == "" && vscsLocation != "" {
		userInputs.Location = vscsLocation
	}

	a.StartProgressIndicatorWithLabel("Fetching repository")
	repository, err := a.apiClient.GetRepository(ctx, userInputs.Repository)
	a.StopProgressIndicator()
	if err != nil {
		return fmt.Errorf("error getting repository: %w", err)
	}

	branch := userInputs.Branch
	if branch == "" {
		branch = repository.DefaultBranch
	}

	machine, err := getMachineName(ctx, a.apiClient, repository.ID, opts.machine, branch, userInputs.Location)
	if err != nil {
		return fmt.Errorf("error getting machine type: %w", err)
	}
	if machine == "" {
		return errors.New("there are no available machine types for this repository")
	}

	createParams := &api.CreateCodespaceParams{
		RepositoryID:       repository.ID,
		Branch:             branch,
		Machine:            machine,
		Location:           userInputs.Location,
		VSCSTarget:         vscsTarget,
		VSCSTargetURL:      vscsTargetUrl,
		IdleTimeoutMinutes: int(opts.idleTimeout.Minutes()),
		PermissionsOptOut:  opts.permissionsOptOut,
	}

	a.StartProgressIndicatorWithLabel("Creating codespace")
	codespace, err := a.apiClient.CreateCodespace(ctx, createParams)
	a.StopProgressIndicator()

	if err != nil {
		var aerr api.AcceptPermissionsRequiredError
		if !errors.As(err, &aerr) || aerr.AllowPermissionsURL == "" {
			return fmt.Errorf("error creating codespace: %w", err)
		}

		codespace, err = a.handleAdditionalPermissions(ctx, createParams, aerr.AllowPermissionsURL)
		if err != nil {
			// this error could be a cmdutil.SilentError (in the case that the user opened the browser) so we don't want to wrap it
			return err
		}
	}

	if opts.showStatus {
		if err := a.showStatus(ctx, codespace); err != nil {
			return fmt.Errorf("show status: %w", err)
		}
	}

	cs := a.io.ColorScheme()

	fmt.Fprintln(a.io.Out, codespace.Name)

	if a.io.IsStderrTTY() && codespace.IdleTimeoutNotice != "" {
		fmt.Fprintln(a.io.ErrOut, cs.Yellow("Notice:"), codespace.IdleTimeoutNotice)
	}

	return nil
}

func (a *App) handleAdditionalPermissions(ctx context.Context, createParams *api.CreateCodespaceParams, allowPermissionsURL string) (*api.Codespace, error) {
	var (
		isInteractive = a.io.CanPrompt()
		cs            = a.io.ColorScheme()
		displayURL    = utils.DisplayURL(allowPermissionsURL)
	)

	fmt.Fprintf(a.io.ErrOut, "You must authorize or deny additional permissions requested by this codespace before continuing.\n")

	if !isInteractive {
		fmt.Fprintf(a.io.ErrOut, "%s in your browser to review and authorize additional permissions: %s\n", cs.Bold("Open this URL"), displayURL)
		fmt.Fprintf(a.io.ErrOut, "Alternatively, you can run %q with the %q option to continue without authorizing additional permissions.\n", a.io.ColorScheme().Bold("create"), cs.Bold("--default-permissions"))
		return nil, cmdutil.SilentError
	}

	choices := []string{
		"Continue in browser to review and authorize additional permissions (Recommended)",
		"Continue without authorizing additional permissions",
	}

	permsSurvey := []*survey.Question{
		{
			Name: "accept",
			Prompt: &survey.Select{
				Message: "What would you like to do?",
				Options: choices,
				Default: choices[0],
			},
			Validate: survey.Required,
		},
	}

	var answers struct {
		Accept string
	}

	if err := ask(permsSurvey, &answers); err != nil {
		return nil, fmt.Errorf("error getting answers: %w", err)
	}

	// if the user chose to continue in the browser, open the URL
	if answers.Accept == choices[0] {
		fmt.Fprintln(a.io.ErrOut, "Please re-run the create request after accepting permissions in the browser.")
		if err := a.browser.Browse(allowPermissionsURL); err != nil {
			return nil, fmt.Errorf("error opening browser: %w", err)
		}
		// browser opened successfully but we do not know if they accepted the permissions
		// so we must exit and wait for the user to attempt the create again
		return nil, cmdutil.SilentError
	}

	// if the user chose to create the codespace without the permissions,
	// we can continue with the create opting out of the additional permissions
	createParams.PermissionsOptOut = true

	a.StartProgressIndicatorWithLabel("Creating codespace")
	codespace, err := a.apiClient.CreateCodespace(ctx, createParams)
	a.StopProgressIndicator()

	if err != nil {
		return nil, fmt.Errorf("error creating codespace: %w", err)
	}

	return codespace, nil
}

// showStatus polls the codespace for a list of post create states and their status. It will keep polling
// until all states have finished. Once all states have finished, we poll once more to check if any new
// states have been introduced and stop polling otherwise.
func (a *App) showStatus(ctx context.Context, codespace *api.Codespace) error {
	var (
		lastState      codespaces.PostCreateState
		breakNextState bool
	)

	finishedStates := make(map[string]bool)
	ctx, stopPolling := context.WithCancel(ctx)
	defer stopPolling()

	poller := func(states []codespaces.PostCreateState) {
		var inProgress bool
		for _, state := range states {
			if _, found := finishedStates[state.Name]; found {
				continue // skip this state as we've processed it already
			}

			if state.Name != lastState.Name {
				a.StartProgressIndicatorWithLabel(state.Name)

				if state.Status == codespaces.PostCreateStateRunning {
					inProgress = true
					lastState = state
					break
				}

				finishedStates[state.Name] = true
				a.StopProgressIndicator()
			} else {
				if state.Status == codespaces.PostCreateStateRunning {
					inProgress = true
					break
				}

				finishedStates[state.Name] = true
				a.StopProgressIndicator()
				lastState = codespaces.PostCreateState{} // reset the value
			}
		}

		if !inProgress {
			if breakNextState {
				stopPolling()
				return
			}
			breakNextState = true
		}
	}

	err := codespaces.PollPostCreateStates(ctx, a, a.apiClient, codespace, poller)
	if err != nil {
		if errors.Is(err, context.Canceled) && breakNextState {
			return nil // we cancelled the context to stop polling, we can ignore the error
		}

		return fmt.Errorf("failed to poll state changes from codespace: %w", err)
	}

	return nil
}

// getMachineName prompts the user to select the machine type, or validates the machine if non-empty.
func getMachineName(ctx context.Context, apiClient apiClient, repoID int, machine, branch, location string) (string, error) {
	machines, err := apiClient.GetCodespacesMachines(ctx, repoID, branch, location)
	if err != nil {
		return "", fmt.Errorf("error requesting machine instance types: %w", err)
	}

	// if user supplied a machine type, it must be valid
	// if no machine type was supplied, we don't error if there are no machine types for the current repo
	if machine != "" {
		for _, m := range machines {
			if machine == m.Name {
				return machine, nil
			}
		}

		availableMachines := make([]string, len(machines))
		for i := 0; i < len(machines); i++ {
			availableMachines[i] = machines[i].Name
		}

		return "", fmt.Errorf("there is no such machine for the repository: %s\nAvailable machines: %v", machine, availableMachines)
	} else if len(machines) == 0 {
		return "", nil
	}

	if len(machines) == 1 {
		// VS Code does not prompt for machine if there is only one, this makes us consistent with that behavior
		return machines[0].Name, nil
	}

	machineNames := make([]string, 0, len(machines))
	machineByName := make(map[string]*api.Machine)
	for _, m := range machines {
		machineName := buildDisplayName(m.DisplayName, m.PrebuildAvailability)
		machineNames = append(machineNames, machineName)
		machineByName[machineName] = m
	}

	machineSurvey := []*survey.Question{
		{
			Name: "machine",
			Prompt: &survey.Select{
				Message: "Choose Machine Type:",
				Options: machineNames,
				Default: machineNames[0],
			},
			Validate: survey.Required,
		},
	}

	var machineAnswers struct{ Machine string }
	if err := ask(machineSurvey, &machineAnswers); err != nil {
		return "", fmt.Errorf("error getting machine: %w", err)
	}

	selectedMachine := machineByName[machineAnswers.Machine]

	return selectedMachine.Name, nil
}

func getRepoSuggestions(ctx context.Context, apiClient apiClient, partialSearch string) []string {
	searchParams := api.RepoSearchParameters{
		// The prompt shows 7 items so 7 effectively turns off scrolling which is similar behavior to other clients
		MaxRepos: 7,
		Sort:     "repo",
	}

	repos, err := apiClient.GetCodespaceRepoSuggestions(ctx, partialSearch, searchParams)
	if err != nil {
		return nil
	}

	return repos
}

// buildDisplayName returns display name to be used in the machine survey prompt.
func buildDisplayName(displayName string, prebuildAvailability string) string {
	prebuildText := ""

	if prebuildAvailability == "blob" || prebuildAvailability == "pool" {
		prebuildText = " (Prebuild ready)"
	}

	return fmt.Sprintf("%s%s", displayName, prebuildText)
}
