package update

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

// Main implements `collie update`: show the running and the latest
// version. (`collie update apply` is the internal apply step.)
func Main(args []string, current, defaultRepo, defaultData string) int {
	if len(args) > 0 && args[0] == "apply" {
		return runApply(args[1:])
	}
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	data := fs.String("data", defaultData, "data directory")
	repo := fs.String("update-repo", defaultRepo, "GitHub repository (owner/name) releases come from")
	_ = fs.Parse(args)
	u := New(Config{Repo: *repo, DataDir: *data, Current: current, Keys: Keys()})
	fmt.Printf("collie %s\n", current)
	if *repo == "" {
		fmt.Println("update checks are turned off (--update-repo)")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := u.Check(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st := u.Status()
	if !st.Available {
		fmt.Printf("up to date (latest release: %s)\n", st.Latest)
		return 0
	}
	fmt.Printf("update available: %s (%s)\n", st.Latest, st.URL)
	fmt.Println("install it from Admin → Updates in the web UI")
	if st.Last != nil && !st.Last.OK {
		fmt.Printf("last update attempt (%s → %s) failed: %s\n", st.Last.From, st.Last.To, st.Last.Error)
	}
	return 0
}
