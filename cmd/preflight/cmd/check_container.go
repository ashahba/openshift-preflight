package cmd

import (
	"context"
	"fmt"
	rt "runtime"
	"strings"

	"github.com/redhat-openshift-ecosystem/openshift-preflight/artifacts"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/certification"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/container"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/internal/check"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/internal/cli"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/internal/formatters"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/internal/lib"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/internal/runtime"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/internal/viper"
	"github.com/redhat-openshift-ecosystem/openshift-preflight/version"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var submit bool

// runPreflight is introduced to make testing of this command possible, it has the same method signature as cli.RunPreflight.
type runPreflight func(context.Context, func(ctx context.Context) (certification.Results, error), cli.CheckConfig, formatters.ResponseFormatter, lib.ResultWriter, lib.ResultSubmitter) error

func checkContainerCmd(runpreflight runPreflight) *cobra.Command {
	checkContainerCmd := &cobra.Command{
		Use:   "container",
		Short: "Run checks for a container",
		Long:  `This command will run the Certification checks for a container image. `,
		Args:  checkContainerPositionalArgs,
		// this fmt.Sprintf is in place to keep spacing consistent with cobras two spaces that's used in: Usage, Flags, etc
		Example: fmt.Sprintf("  %s", "preflight check container quay.io/repo-name/container-name:version"),
		PreRunE: validateCertificationProjectID,
		RunE: func(cmd *cobra.Command, args []string) error {
			return checkContainerRunE(cmd, args, runpreflight)
		},
	}

	flags := checkContainerCmd.Flags()

	viper := viper.Instance()
	flags.BoolVarP(&submit, "submit", "s", false, "submit check container results to Red Hat")
	_ = viper.BindPFlag("submit", flags.Lookup("submit"))

	flags.Bool("insecure", false, "Use insecure protocol for the registry. Default is False. Cannot be used with submit.")
	_ = viper.BindPFlag("insecure", flags.Lookup("insecure"))

	// Make --submit mutually exclusive to --insecure
	checkContainerCmd.MarkFlagsMutuallyExclusive("submit", "insecure")

	flags.String("pyxis-api-token", "", "API token for Pyxis authentication (env: PFLT_PYXIS_API_TOKEN)")
	_ = viper.BindPFlag("pyxis_api_token", flags.Lookup("pyxis-api-token"))

	flags.String("pyxis-host", "", fmt.Sprintf("Host to use for Pyxis submissions. This will override Pyxis Env. Only set this if you know what you are doing.\n"+
		"If you do set it, it should include just the host, and the URI path. (env: PFLT_PYXIS_HOST)"))
	_ = viper.BindPFlag("pyxis_host", flags.Lookup("pyxis-host"))

	flags.String("pyxis-env", check.DefaultPyxisEnv, "Env to use for Pyxis submissions.")
	_ = viper.BindPFlag("pyxis_env", flags.Lookup("pyxis-env"))

	flags.String("certification-project-id", "", fmt.Sprintf("Certification Project ID from connect.redhat.com/projects/{certification-project-id}/overview\n"+
		"URL paramater. This value may differ from the PID on the overview page. (env: PFLT_CERTIFICATION_PROJECT_ID)"))
	_ = viper.BindPFlag("certification_project_id", flags.Lookup("certification-project-id"))

	checkContainerCmd.Flags().String("platform", rt.GOARCH, "Architecture of image to pull. Defaults to current platform.")
	_ = viper.BindPFlag("platform", checkContainerCmd.Flags().Lookup("platform"))

	return checkContainerCmd
}

// checkContainerRunE executes checkContainer using the user args to inform the execution.
func checkContainerRunE(cmd *cobra.Command, args []string, runpreflight runPreflight) error {
	ctx := cmd.Context()
	logger, err := logr.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("invalid logging configuration")
	}
	logger.Info("certification library version", "version", version.Version.String())

	containerImage := args[0]

	// Render the Viper configuration as a runtime.Config
	cfg, err := runtime.NewConfigFrom(*viper.Instance())
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	artifactsWriter, err := artifacts.NewFilesystemWriter(artifacts.WithDirectory(cfg.Artifacts))
	if err != nil {
		return err
	}

	// Add the artifact writer to the context for use by checks.
	ctx = artifacts.ContextWithWriter(ctx, artifactsWriter)

	formatter, err := formatters.NewByName(formatters.DefaultFormat)
	if err != nil {
		return err
	}

	opts := generateContainerCheckOptions(cfg)

	checkcontainer := container.NewCheck(
		containerImage,
		opts...,
	)

	pc := lib.NewPyxisClient(ctx, cfg.CertificationProjectID, cfg.PyxisAPIToken, cfg.PyxisHost)
	resultSubmitter := lib.ResolveSubmitter(pc, cfg.CertificationProjectID, cfg.DockerConfig, cfg.LogFile)

	// Run the  container check.
	cmd.SilenceUsage = true

	return runpreflight(
		ctx,
		checkcontainer.Run,
		cli.CheckConfig{
			IncludeJUnitResults: cfg.WriteJUnit,
			SubmitResults:       cfg.Submit,
		},
		formatter,
		&runtime.ResultWriterFile{},
		resultSubmitter,
	)
}

func checkContainerPositionalArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("a container image positional argument is required")
	}

	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed && strings.Contains(f.Value.String(), "--submit") {
			// We have --submit in one of the flags. That's a problem.
			// We will set the submit flag to true so that the next block functions properly
			submit = true
		}
	})

	// --submit was specified
	viper := viper.Instance()
	if submit {
		// If the flag is not marked as changed AND viper hasn't gotten it from environment, it's an error
		if !cmd.Flag("certification-project-id").Changed && !viper.IsSet("certification_project_id") {
			return fmt.Errorf("certification Project ID must be specified when --submit is present")
		}
		if !cmd.Flag("pyxis-api-token").Changed && !viper.IsSet("pyxis_api_token") {
			return fmt.Errorf("pyxis API Token must be specified when --submit is present")
		}

		// If the flag is marked as changed AND it's still empty, it's an error
		if cmd.Flag("certification-project-id").Changed && viper.GetString("certification_project_id") == "" {
			return fmt.Errorf("certification Project ID cannot be empty when --submit is present")
		}
		if cmd.Flag("pyxis-api-token").Changed && viper.GetString("pyxis_api_token") == "" {
			return fmt.Errorf("pyxis API Token cannot be empty when --submit is present")
		}

		// Finally, if either certification project id or pyxis api token start with '--', it's an error
		if strings.HasPrefix(viper.GetString("pyxis_api_token"), "--") || strings.HasPrefix(viper.GetString("certification_project_id"), "--") {
			return fmt.Errorf("pyxis API token and certification ID are required when --submit is present")
		}
	}

	return nil
}

// validateCertificationProjectID validates that the certification project id is in the proper format
// and throws an error if the value provided is in a legacy format that is not usable to query pyxis
func validateCertificationProjectID(cmd *cobra.Command, args []string) error {
	viper := viper.Instance()
	certificationProjectID := viper.GetString("certification_project_id")
	// splitting the certification project id into parts. if there are more than 2 elements in the array,
	// we know they inputted a legacy project id, which can not be used to query pyxis
	parts := strings.Split(certificationProjectID, "-")

	if len(parts) > 2 {
		return fmt.Errorf("certification project id: %s is improperly formatted see help command for instructions on obtaining proper value", certificationProjectID)
	}

	if parts[0] == "ospid" {
		viper.Set("certification_project_id", parts[1])
	}

	return nil
}

// generateContainerCheckOptions returns appropriate container.Options based on cfg.
func generateContainerCheckOptions(cfg *runtime.Config) []container.Option {
	o := []container.Option{
		container.WithCertificationProject(cfg.CertificationProjectID, cfg.PyxisAPIToken),
		container.WithDockerConfigJSONFromFile(cfg.DockerConfig),
		// Always add PyxisHost, since the value is always set in viper config parsing.
		container.WithPyxisHost(cfg.PyxisHost),
		container.WithPlatform(cfg.Platform),
	}

	// set auth information if both are present in config.
	if cfg.PyxisAPIToken != "" && cfg.CertificationProjectID != "" {
		o = append(o, container.WithCertificationProject(cfg.CertificationProjectID, cfg.PyxisAPIToken))
	}

	if cfg.Insecure {
		// Do not allow for submission if Insecure is set.
		// This is a secondary check to be safe.
		cfg.Submit = false
		o = append(o, container.WithInsecureConnection())
	}

	return o
}
