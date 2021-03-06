// watchurl checks a URL for updates and logs a pretty diff.
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
	"runtime"
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
	stateDir       = flag.String("state-dir", "~/.watchurl/", "directory where to cache site contents")
	every          = flag.Duration("repeat-every", 0, "keep running, checking at this interval")
	jitter         = flag.Duration("jitter", 2*time.Minute, "random jitter, if --repeat-every is used")
	macNotify      = flag.Bool("macos-notify", true, "(macOS only) display a desktop notification when updated")
	macClipboard   = flag.Bool("macos-clipboard", false, "(macOS only) put the latest URL to update in the clipboard")
	logFullDiff    = flag.Bool("log-full-diff", false, "Write the full diff to glog (otherwise write it to stdout)")
	requestTimeout = flag.Duration("request-timeout", 30*time.Second, "timeout for the HTTP GET requests (0 to disable)")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] URLS...\n\n", os.Args[0])
		fmt.Fprint(os.Stderr, "Monitors URLs for updates and outputs diffs.\n\n")
		fmt.Fprint(os.Stderr, "EXAMPLE:\n")
		fmt.Fprint(os.Stderr, "# Check major news outlets every 5 minutes:\n")
		fmt.Fprintf(os.Stderr, "%s --repeat-every=5m --log-full-diff https://theguardian.com https://nytimes.com\n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()
	urls := flag.Args()

	if runtime.GOOS != "darwin" {
		flag.Set("macos-notify", "0")
	}

	if !(flagIsSet("logtostderr") || flagIsSet("alsologtostderr")) && *logFullDiff {
		flag.Set("alsologtostderr", "1")
	}

	if *every > 0 {
		glog.Infof("Will check %d URLs for updates every %v (+ jitter up to %v)", len(urls), *every, *jitter)
	} else {
		glog.Infof("Will check %d URLs for updates ONCE (use --repeat-every to keep checking)", len(urls))
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Buffered to two values, because a SIGTERM might come from the system, or
	// when all the goroutines finish, and we only drain the channel once, so
	// one of those might theoretically block.
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	// There are two ways to shut down. Firstly, if we get SIGTERM, then ch will
	// unblock, we then cancel the context and wait for done to close. Secondly,
	// if all the goroutines finish before SIGTERM (because *every == 0), then
	// both ch and done will unblock and we'll shut down immediately.
	var wg sync.WaitGroup
	wg.Add(len(urls))
	for _, addr := range urls {
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

func flagIsSet(name string) bool {
	isSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			isSet = true
		}
	})
	return isSet
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

			diff, edits, err := diffURLWithTimeout(ctx, *requestTimeout, addr)

			if err != nil {
				glog.Warningf("Checking %s: %v", addr, err)
				continue
			}
			if diff != "" {
				notify(addr, diff, edits)
			} else {
				glog.V(1).Infof("No change in %s", addr)
			}

			if every <= 0 {
				glog.Info("Bailing after a successful check (use --repeat-every to repeat automatically)")
				return
			}
		}
	}
}

func notify(addr, diff string, edits int) {
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
	if *macClipboard {
		mack.SetClipboard(addr)
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
	if len(name) > 127 { // Conservative limit
		name = name[:127]
	}
	return filepath.Join(dir, name), nil
}

func writeState(addr, text string) error {
	name, err := statePath(addr)
	if err != nil {
		return err
	}
	glog.V(2).Infof("Address %s stored in %s (%d bytes)", addr, name, len(text))
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
	glog.V(2).Infof("Address %s snapshot loaded from %s (%d bytes)", addr, name, len(p))
	return string(p), nil
}

func diffURLWithTimeout(ctx context.Context, timeout time.Duration, addr string) (string, int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *requestTimeout)
		defer cancel()
	}

	diff, edits, err := diffURL(ctx, addr)
	if err == nil && ctx.Err() != nil {
		err = fmt.Errorf("checking %s, context error: %w", addr, ctx.Err())
	}
	return diff, edits, err
}

func diffURL(ctx context.Context, addr string) (string, int, error) {
	text, err := getURLText(ctx, addr)
	if err != nil {
		return "", 0, err
	}
	old, err := readState(addr)
	if os.IsNotExist(err) {
		glog.Infof("First time checking %s (no previous state)", addr)
		err = nil
	}
	if err != nil {
		return "", 0, err
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
