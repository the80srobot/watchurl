# WatchURL

Checks URLs for updates and prints human-readable diffs to the console or to a
logfile. Maintains states between runs.

Requires go 1.16. If you don't want to install it, then use `run_in_docker.sh`
instead of `go run watchurl.go`.

## macOS notifications

On macOS, watchurl shows desktop notifications when a site is updated. To
disable this behavior, pass `--nomacos-notify`.

## Examples

```sh
# Check major news outlets every 5 minutes (prints updates to STDERR and a permanent log file)
go run watchurl.go --log-full-diff --every=5m https://theguardian.com https://nytimes.com
```

```sh
# Check major news outlets every 5 minutes (prints updates to STDOUT)
go run watchurl.go --every=5m https://theguardian.com https://nytimes.com
```

```sh
# Check websites for updates once, writing to a logfile. (Suitable as a cronjob.)
# NOTE: the --log_dir target must exist, it won't be created automatically.
go run watchurl.go --log-full-diff --log_dir=$HOME/watchurl_logs https://theguardian.com https://nytimes.com
```