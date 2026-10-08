#!/bin/sh
# release-smoke.sh checks that a released MailRules runs: the release verify workflow
# (.github/workflows/verify-release.yml) calls it for every published format.
#
#   release-smoke.sh bin PATH VERSION   start PATH serve on a fresh data dir, then check it
#   release-smoke.sh url URL VERSION    check a MailRules already listening at URL
#
# The checks: /healthz answers, /metrics names VERSION, the web UI is built in (the page
# loads its script bundle, which is served), and a fresh install asks for first-run setup.
set -eu

mode=$1 target=$2 want=$3

fail() {
	echo "release-smoke: $*" >&2
	if [ -n "${log:-}" ] && [ -f "$log" ]; then
		echo "--- daemon log" >&2
		cat "$log" >&2
	fi
	exit 1
}

case $mode in
bin)
	got=$("$target" version) || fail "$target version failed"
	[ "$got" = "$want" ] || fail "$target version printed $got, want $want"
	dir=$(mktemp -d)
	log=$dir/log
	port=${SMOKE_PORT:-18080}
	MAILRULES_DATA_DIR=$dir/data MAILRULES_LISTEN=127.0.0.1:$port "$target" serve >"$log" 2>&1 &
	pid=$!
	trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; rm -rf "$dir"' EXIT
	base=http://127.0.0.1:$port
	;;
url)
	base=$target
	;;
*)
	fail "unknown mode $mode (want bin or url)"
	;;
esac

up=
for _ in $(seq 1 60); do
	if curl -fs -o /dev/null "$base/healthz"; then
		up=1
		break
	fi
	sleep 1
done
[ -n "$up" ] || fail "$base/healthz did not answer within 60 s"

curl -fsS "$base/metrics" | grep -q "mailrules_build_info{version=\"$want\"}" ||
	fail "$base/metrics does not name version $want"

page=$(curl -fsS "$base/")
asset=$(printf '%s' "$page" | grep -oE '/assets/index-[A-Za-z0-9_-]+\.js' | head -n 1)
[ -n "$asset" ] || fail "$base/ is not the built web app (no /assets/index-*.js)"
curl -fsS -o /dev/null "$base$asset" || fail "$base$asset is not served"

me=$(curl -sS "$base/api/auth/me")
printf '%s' "$me" | grep -q '"setup_required"' || fail "a fresh install did not ask for setup: $me"

echo "release-smoke: $mode $target $want ok"
