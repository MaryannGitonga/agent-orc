#!/usr/bin/env bash
# Run three tasks end to end and watch them in the dashboard.
#
# Everything is real except the model. The worktrees, the detached supervisors,
# the test gate, the review, the commit sanitization and the state records are
# agent-orc doing exactly what it does for real work. The agent is a stand-in
# claude script on PATH that edits a file and commits, so this costs nothing,
# needs no network and no credentials, and finishes in about half a minute.
#
# No real agent CLI is ever started: the stand-in shadows claude, the review is
# pointed at it too, and the run refuses to start if anything else would answer
# to that name.
#
#   make demo              opens the dashboard
#   DEMO_HEADLESS=1 make demo
#                          waits, then prints the outcome instead
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
orc="$root/bin/agent-orc"
tui="$root/bin/agent-orc-tui"
for bin in "$orc" "$tui"; do
	[ -x "$bin" ] || { echo "missing $bin; run 'make demo', which builds it" >&2; exit 1; }
done

demo="$(mktemp -d -t agent-orc-demo.XXXXXX)"
repo="$demo/repo"
stubs="$demo/bin"
export AGENT_ORC_HOME="$demo/home"
mkdir -p "$repo" "$stubs" "$AGENT_ORC_HOME"

# A repository with a test suite agent-orc can discover: a make test target.
# The suite fails while the greeting still contains BUG, which is what gives
# the first task something to get wrong and then fix.
git -C "$repo" init -q -b main
git -C "$repo" config user.name "agent-orc demo"
git -C "$repo" config user.email "demo@example.invalid"
# Local to this throwaway repository, and to the worktrees made from it: your
# own signing and hooks have no business running on a demo's commits.
git -C "$repo" config commit.gpgsign false
git -C "$repo" config core.hooksPath /dev/null
printf 'hello\n' > "$repo/greeting.txt"
cat > "$repo/check.sh" <<'EOF'
#!/bin/sh
if grep -q BUG greeting.txt; then
	echo "FAIL greeting.txt still contains BUG"
	exit 1
fi
echo "ok"
EOF
printf 'test:\n\t@sh check.sh\n' > "$repo/Makefile"
git -C "$repo" add -A
git -C "$repo" commit -q -m "chore: start the demo repository"

# The stand-in agent. It reads what agent-orc passes a real claude, does a
# small, visible piece of work per task, and answers in claude's JSON result
# format so the log summary and the spend column have something to show.
cat > "$stubs/claude" <<'EOF'
#!/bin/sh
prompt=""; resume=""; session="demo-session"
while [ $# -gt 0 ]; do
	case "$1" in
		-p) prompt="$2"; shift 2 ;;
		--resume) resume=1; session="$2"; shift 2 ;;
		--session-id) session="$2"; shift 2 ;;
		*) shift ;;
	esac
done

answer() { # result, cost, turns, milliseconds
	printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","total_cost_usd":%s,"num_turns":%s,"duration_ms":%s,"session_id":"%s"}\n' \
		"$1" "$2" "$3" "$4" "$session"
}
commit() {
	git add -A
	git commit -q --no-gpg-sign -m "$1"
}

case "$prompt" in
	"You are reviewing a change"*)
		sleep 5
		answer "LGTM" 0.0310 3 5000
		exit 0 ;;
esac

# Handed back after a failed test run: the fix for whatever it got wrong.
if [ -n "$resume" ]; then
	sleep 5
	if grep -q BUG greeting.txt 2>/dev/null; then
		printf 'hello, world\n' > greeting.txt
		commit "fix: repair the greeting"
	fi
	answer "Fixed the greeting so the suite passes." 0.0920 6 5000
	exit 0
fi

case "$prompt" in
	*greeting*)
		sleep 7
		printf 'hello BUG\n' > greeting.txt
		# The trailer a real agent tends to add, so the sanitization pass has
		# something to remove before anything is published.
		commit "$(printf 'feat: extend the greeting\n\nCo-authored-by: Claude <noreply@anthropic.com>')"
		answer "Extended the greeting." 0.2140 9 7000 ;;
	*farewell*)
		sleep 10
		printf 'goodbye\n' > farewell.txt
		commit "feat: add a farewell"
		answer "Added a farewell." 0.1580 7 10000 ;;
	*)
		sleep 4
		echo "error: the stand-in agent was asked to fail" >&2
		exit 1 ;;
esac
EOF
chmod +x "$stubs/claude"
export PATH="$stubs:$PATH"

# The guard: a real claude answering here would spend real money.
if [ "$(command -v claude)" != "$stubs/claude" ]; then
	echo "refusing to run: 'claude' resolves to $(command -v claude), not the demo's stand-in" >&2
	exit 1
fi

echo "dispatching three tasks against a scratch repository in $demo"
"$orc" run --id DEMO-1 --cli claude --repo "$repo" --budget-usd 1 \
	--auto-review --review-cli claude \
	--prompt "extend the greeting" >/dev/null
sleep 1
"$orc" run --id DEMO-2 --cli claude --repo "$repo" --budget-usd 1 \
	--prompt "add a farewell" >/dev/null
sleep 1
"$orc" run --id DEMO-3 --cli claude --repo "$repo" --budget-usd 1 \
	--prompt "a task the stand-in will fail" >/dev/null

finished() {
	"$orc" status | grep -qE ' (pending|running|verifying|reviewing|publishing) ' && return 1
	return 0
}

if [ -t 1 ] && [ -z "${DEMO_HEADLESS:-}" ]; then
	"$tui"
else
	for _ in $(seq 1 90); do
		finished && break
		sleep 1
	done
	"$orc" status
fi

cat <<EOF

The tasks ran against a real repository. What they left behind:

  the greeting task's branch, trailer stripped by sanitization:
    git -C $repo log --format='%h %s%n%b' agent-orc/demo-1

  the supervisor's account of the test loop and the review:
    cat $AGENT_ORC_HOME/logs/DEMO-1.supervisor.log

  remove it all:
    rm -rf $demo
EOF
