// Command shunter-replay runs train logs through the state machine and
// reports the histories it refuses.
//
//	shunter-replay [-stats stats.jsonl] [-v] train-log...
//
// Logs are replayed in order of file name.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/rolandpj1968/shunter/replay"
	"github.com/rolandpj1968/shunter/replay/adapter/trainlog"
	"github.com/rolandpj1968/shunter/state"
)

func main() {
	stats := flag.String("stats", "", "a JSON-lines stats file; rows with stage \"landed\" say which trains landed")
	verbose := flag.Bool("v", false, "list every finding")
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	paths := flag.Args()
	sort.Slice(paths, func(i, j int) bool { return filepath.Base(paths[i]) < filepath.Base(paths[j]) })
	src := &trainlog.Source{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			fatal(err)
		}
		lg, err := trainlog.Parse(filepath.Base(p), f)
		f.Close()
		if err != nil {
			fatal(fmt.Errorf("%s: %w", p, err))
		}
		src.Logs = append(src.Logs, lg)
	}
	if *stats != "" {
		f, err := os.Open(*stats)
		if err != nil {
			fatal(err)
		}
		landed, err := trainlog.LandedPRs(f)
		f.Close()
		if err != nil {
			fatal(err)
		}
		src.Landed = func(pr string) bool { return landed[pr] }
	}
	s, rep := replay.Run(state.New("trunk-at-start", state.Config{}), src)
	fmt.Printf("logs %d, of which %d have no protocol line naming a change\n", len(src.Logs), len(src.Unrecorded))
	rep.Write(os.Stdout, *verbose)
	count := map[string]int{}
	for _, l := range s.Lines {
		count[l.State.String()]++
	}
	fmt.Printf("lines at the end: %v; landings %d\n", count, len(s.Landings))
	if len(src.Ignored) > 0 {
		fmt.Printf("marker words not understood: %v\n", src.Ignored)
	}
	if *verbose {
		for _, n := range src.Unrecorded {
			fmt.Printf("  unrecorded %s\n", n)
		}
	}
	if len(rep.Breaches) > 0 {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "shunter-replay:", err)
	os.Exit(1)
}
