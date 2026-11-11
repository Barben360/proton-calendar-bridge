package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/seed"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

const defaultDataset = "testdata/seed/dataset.yaml"

func seedCmd() *cobra.Command {
	c := cli.Group("seed", "Plan or apply the synthetic data set on the test account through proton-cal",
		seedPlanCmd(), seedApplyCmd())
	c.PersistentFlags().String("dataset", defaultDataset, "data set file")
	return c
}

func loadDataset(cmd *cobra.Command) (seed.Dataset, error) {
	path, _ := cmd.Flags().GetString("dataset")
	ds, err := seed.Load(path)
	if err != nil {
		return ds, cli.UsageError(err)
	}
	return ds, nil
}

type planCommand struct {
	ID       string   `json:"id"`
	Op       string   `json:"op"`
	Ref      string   `json:"ref,omitempty"`
	Calendar string   `json:"calendar"`
	Args     []string `json:"args"`
}

type planData struct {
	Commands []planCommand `json:"commands"`
	Total    int           `json:"total"`
}

// seedPlanCmd works fully offline: "existing" events come from a JSON file
// (`proton-cal events --all-calendars -o json`), never from the network.
func seedPlanCmd() *cobra.Command {
	var existingFile string
	c := &cobra.Command{
		Use:   "plan",
		Short: "Print the proton-cal commands still needed (offline)",
		Args:  cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds, err := loadDataset(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			var existing []seed.Existing
			if existingFile != "" {
				f, err := os.Open(existingFile)
				if err != nil {
					return cli.Finish(cmd, nil, "", cli.UsageError(err))
				}
				defer func() { _ = f.Close() }()
				if existing, err = seed.ParseExisting(f); err != nil {
					return cli.Finish(cmd, nil, "", cli.UsageError(err))
				}
			}
			cmds := seed.Plan(ds, existing)
			data := planData{Commands: make([]planCommand, 0, len(cmds)), Total: len(cmds)}
			var text strings.Builder
			for _, p := range cmds {
				data.Commands = append(data.Commands, planCommand{p.ID, p.Op, p.Ref, p.Calendar, p.Args})
				quoted := make([]string, len(p.Args))
				for i, a := range p.Args {
					quoted[i] = shellquote.Quote(a)
				}
				fmt.Fprintf(&text, "proton-cal %s\n", strings.Join(quoted, " "))
			}
			fmt.Fprintf(&text, "# %d commands; {cal} and {id} are filled in by `seed apply`", len(cmds))
			return cli.Finish(cmd, data, text.String(), nil)
		},
	}
	c.Flags().StringVar(&existingFile, "existing", "", "JSON listing of existing events (proton-cal events --all-calendars -o json) to skip")
	return c
}

type applyData struct {
	Written int `json:"written"`
	Planned int `json:"planned"`
}

func seedApplyCmd() *cobra.Command {
	var bin, configDir string
	var interval time.Duration
	c := &cobra.Command{
		Use:   "apply",
		Short: "Create the data set on the test account with proton-cal (one write per --interval, stops on the first 429)",
		Long: `Runs proton-cal (already logged in, see spec/experiments/DATASET.md) against the
test account: resolves the default calendar by its default flag, creates the
missing calendars, skips what the data set already created, then writes the rest
in order. It never logs in itself. Rerun after a stop: it resumes.`,
		Args: cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds, err := loadDataset(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			if configDir == "" {
				dir, err := stateDir(cmd)
				if err != nil {
					return cli.Finish(cmd, nil, "", err)
				}
				configDir = filepath.Join(filepath.Dir(dir), "pcb-lab-seed")
			}
			runner := &seed.ExecRunner{Bin: bin, ConfigDir: configDir}
			res, err := seed.Apply(cmd.Context(), ds, runner, seed.ApplyOptions{Wait: interval, Log: cmd.ErrOrStderr()})
			data := applyData{res.Written, res.Planned}
			switch {
			case err == nil:
				return cli.Finish(cmd, data, fmt.Sprintf("seeded: %d writes", res.Written), nil)
			case errors.Is(err, seed.ErrRateLimited):
				err = &exitcode.Error{Code: exitcode.Partial, Err: err,
					Hint: "wait a few minutes, then rerun the same `pcb-lab seed apply`: what exists is skipped and the rest resumes"}
			case res.Written > 0:
				err = &exitcode.Error{Code: exitcode.Partial, Err: err, Hint: "fix the cause, then rerun the same `pcb-lab seed apply`: it resumes"}
			default:
				err = &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: "is proton-cal logged in with this --config-dir? See spec/experiments/DATASET.md"}
			}
			return cli.Finish(cmd, data, fmt.Sprintf("%d writes done", res.Written), err)
		},
	}
	c.Flags().StringVar(&bin, "bin", "proton-cal", "proton-cal binary")
	c.Flags().StringVar(&configDir, "config-dir", "", "proton-cal config dir (default $XDG_STATE_HOME/pcb-lab-seed)")
	c.Flags().DurationVar(&interval, "interval", 2*time.Second, "pause between writes")
	return c
}
