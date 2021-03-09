package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andybrewer/mack"
	"github.com/golang/glog"
	"github.com/sergi/go-diff/diffmatchpatch"
	"jaytaylor.com/html2text"
)

var (
	stateDir    = flag.String("state-dir", "~/.watchurl/", "directory where to cache site contents")
	every       = flag.Duration("repeat-every", 0, "keep running, checking at this interval")
	jitter      = flag.Duration("jitter", 5*time.Minute, "random jitter, if --repeat-every is used")
	macNotify   = flag.Bool("macos-notify", false, "(macOS only) display a desktop notification when updated")
	logFullDiff = flag.Bool("log-full-diff", false, "Write the full diff to glog (otherwise write it to stdout)")
)

func main() {
	flag.Parse()
	ctx, cancel := context.WithCancel(context.Background())
	// Buffered to two values, because a SIGTERM might come from the system, or
	// when all the goroutines finish, and we only drain the channel once, so
	// one of those might theoretically block.
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	args := flag.Args()

	// There are two ways to shut down. Firstly, if we get SIGTERM, then ch will
	// unblock, we then cancel the context and wait for done to close. Secondly,
	// if all the goroutines finish before SIGTERM (because *every == 0), then
	// both ch and done will unblock and we'll shut down immediately.
	var wg sync.WaitGroup
	wg.Add(len(args))
	for _, addr := range args {
		go func(addr string) {
			defer wg.Done()
			watch(ctx, addr, *every, *jitter)
		}(addr)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		ch <- syscall.SIGTERM
		close(done)
	}()

	<-ch
	glog.Info("Shutting down...")
	cancel()
	<-done
}

// watch diffs the text of the website at the given url against the last
// snapshot and notifies the user of any updates. If every is positive, then
// watch will keep running until the context is cancelled, checking once every
// interval (plust pseudo-random jitter up to the value of jitter).
func watch(ctx context.Context, addr string, every, jitter time.Duration) {
	t := time.NewTimer(time.Nanosecond)
	defer t.Stop()

	if jitter > 0 && every > 0 {
		rand.Seed(time.Now().UnixNano())
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if every > 0 {
				var j time.Duration
				if jitter > 0 {
					j = time.Duration(rand.Int63n(int64(jitter/time.Nanosecond))) * time.Nanosecond
				}
				t.Reset(every + j)
				glog.V(2).Infof("Fetching %s, then next fetch in %v + %v/%v jitter", addr, every, j, jitter)
			}

			diff, edits, err := diffURL(ctx, addr)
			if err != nil {
				glog.Warningf("Checking %s: %v", addr, err)
				continue
			}
			if diff != "" {
				if *logFullDiff {
					glog.Infof("Site %s updated (%d edits):\n%s", addr, edits, diff)
				} else {
					// Avoid writing the full output to both stdout and log.
					fmt.Printf("Site %s diff:\n%s\n", addr, diff)
					glog.Infof("Site %s updated (%d edits)", addr, edits)
				}
				if *macNotify {
					mack.Notify("Site updated", addr, fmt.Sprintf("%d edits (check console output)", edits), "Ping")
				}

			} else {
				glog.V(1).Infof("No change in %s", addr)
			}

			if every == 0 {
				glog.Info("Bailing after a successful check (use --repeat-every to repeat automatically)")
				return
			}
		}
	}
}

var specialRE = regexp.MustCompile(`[^\w]+`)

func statePath(addr string) (string, error) {
	dir := *stateDir
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = strings.Replace(dir, "~", home, 1)
	}

	digest := sha1.Sum([]byte(addr))
	name := fmt.Sprintf("%s_%s", hex.EncodeToString(digest[:]), specialRE.ReplaceAllLiteralString(addr, "_"))
	return filepath.Join(dir, name), nil
}

func writeState(addr, text string) error {
	name, err := statePath(addr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}

	return os.WriteFile(name, []byte(text), 0644)
}

func readState(addr string) (string, error) {
	name, err := statePath(addr)
	if err != nil {
		return "", err
	}
	p, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(p), nil
}

func diffURL(ctx context.Context, addr string) (string, int, error) {
	text, err := getURLText(ctx, addr)
	if err != nil {
		return "", 0, nil
	}
	old, err := readState(addr)
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return "", 0, nil
	}

	if old == text {
		return "", 0, nil
	}

	if err := writeState(addr, text); err != nil {
		return "", 0, err
	}

	if old == "" {
		return "(initial fetch)", 0, nil
	}

	var sb strings.Builder
	edits := reportDiffs(old, text, &sb)
	return sb.String(), edits, nil
}

func reportDiffs(old, new string, w io.Writer) int {
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffCleanupSemantic(dmp.DiffMain(old, new, true))
	edits := 0

	for _, diff := range diffs {
		switch diff.Type {
		case diffmatchpatch.DiffInsert:
			edits++
			io.WriteString(w, "\x1b[32m")
			io.WriteString(w, diff.Text)
			io.WriteString(w, "\x1b[0m")
		case diffmatchpatch.DiffDelete:
			edits++
			io.WriteString(w, "\x1b[31m")
			io.WriteString(w, diff.Text)
			io.WriteString(w, "\x1b[0m")
		case diffmatchpatch.DiffEqual:
			firstNL := strings.IndexByte(diff.Text, '\n')
			lastNL := strings.LastIndexByte(diff.Text, '\n')
			if lastNL == firstNL {
				io.WriteString(w, diff.Text)
			} else {
				io.WriteString(w, diff.Text[:firstNL])
				fmt.Fprintf(w, "\n(skipped %d bytes)\n", lastNL-firstNL)
				io.WriteString(w, diff.Text[lastNL:])
			}
		}
	}
	return edits
}

func getURLText(ctx context.Context, addr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	text, err := html2text.FromString(string(body), html2text.Options{OmitLinks: true})
	if err != nil {
		return "", err
	}
	return text, nil
}
