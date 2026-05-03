package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/itsHabib/orchestra/internal/credentials"
)

var credentialsCmd = &cobra.Command{
	Use:   "credentials",
	Short: "Manage credentials orchestra injects into agent sessions",
	Long: "Read, write, and inspect the per-user credentials store at\n" +
		"<user-config-dir>/orchestra/credentials.json. The store is a flat\n" +
		"name → secret map; agents declare `requires_credentials:` in their\n" +
		"yaml and orchestra resolves the names against this file at run\n" +
		"start. Env vars override the file (canonical name → upper-snake form,\n" +
		"e.g. github_token → GITHUB_TOKEN).\n\n" +
		"`get` only confirms presence and never prints values — secrets do\n" +
		"not leave the file. `list` prints names only.",
}

var credentialsSetCmd = &cobra.Command{
	Use:   "set <name> [value]",
	Short: "Set a credential by name",
	Long: "Set <name> to <value> in the credentials store. If <value> is omitted,\n" +
		"the value is read from stdin (line-buffered, trailing newline stripped)\n" +
		"so secrets do not appear in shell history. The file is rewritten\n" +
		"atomically with mode 0600 on POSIX hosts.",
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		var value string
		switch len(args) {
		case 2:
			value = args[1]
		default:
			read, err := readSecretFromStdin()
			if err != nil {
				return fmt.Errorf("credentials set: read stdin: %w", err)
			}
			value = read
		}
		if value == "" {
			return errors.New("credentials set: value is empty")
		}
		store := credentials.New("")
		if err := store.Set(cmd.Context(), name, value); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "set credential %q in %s\n", name, store.Path())
		return nil
	},
}

var credentialsGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Confirm presence of a credential without printing the value",
	Long: "Reports whether <name> exists in the store and, if not, whether\n" +
		"the env override is set. Never prints the secret value — use the\n" +
		"file directly if you need to recover the plaintext.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		store := credentials.New("")
		ctx := cmd.Context()
		hasFile, err := store.Has(ctx, name)
		if err != nil {
			return err
		}
		envName := credentials.EnvNameFor(name)
		_, hasEnv := os.LookupEnv(envName)
		out := cmd.OutOrStdout()
		switch {
		case hasFile && hasEnv:
			_, _ = fmt.Fprintf(out, "%s: present (file=%s, env=%s; env wins at run time)\n", name, store.Path(), envName)
		case hasFile:
			_, _ = fmt.Fprintf(out, "%s: present in file %s\n", name, store.Path())
		case hasEnv:
			_, _ = fmt.Fprintf(out, "%s: present via env %s only\n", name, envName)
		default:
			_, _ = fmt.Fprintf(out, "%s: not set (looked in %s and env %s)\n", name, store.Path(), envName)
			return errCredentialMissing
		}
		return nil
	},
}

var credentialsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List credential names (values are never printed)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		store := credentials.New("")
		names, err := store.Names(cmd.Context())
		if err != nil {
			return err
		}
		if len(names) == 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no credentials set in %s\n", store.Path())
			return nil
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s:\n", store.Path())
		for _, name := range names {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", name)
		}
		return nil
	},
}

var credentialsDeleteCmd = &cobra.Command{
	Use:     "delete <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a credential by name",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := credentials.New("")
		if err := store.Delete(cmd.Context(), args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted credential %q from %s\n", args[0], store.Path())
		return nil
	},
}

// errCredentialMissing makes `orchestra credentials get` exit non-zero when
// the credential is absent so shell pipelines / CI can branch on it.
var errCredentialMissing = errors.New("credential not set")

// readSecretFromStdin reads one line from stdin and strips the trailing
// newline. Used by `credentials set <name>` (no value arg) so secrets stay
// out of shell history and `ps` listings.
func readSecretFromStdin() (string, error) {
	data, err := readAllStdin()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(strings.TrimRight(data, "\n"), "\r"), nil
}

func readAllStdin() (string, error) {
	const limit = 64 * 1024
	buf := make([]byte, limit)
	n, err := os.Stdin.Read(buf)
	if err != nil && err.Error() != "EOF" {
		return "", err
	}
	return string(buf[:n]), nil
}

func init() {
	credentialsCmd.AddCommand(credentialsSetCmd)
	credentialsCmd.AddCommand(credentialsGetCmd)
	credentialsCmd.AddCommand(credentialsListCmd)
	credentialsCmd.AddCommand(credentialsDeleteCmd)
	rootCmd.AddCommand(credentialsCmd)
}
