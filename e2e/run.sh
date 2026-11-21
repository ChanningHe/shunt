#!/usr/bin/env bash
# End-to-end check: real Claude Code (subscription login) + shunt + DeepSeek.
# Requires DEEPSEEK_API_KEY in the environment (e.g. sourced from a sops-decrypted
# file by the caller); this script never prints it. It starts shunt on an
# empty throwaway config directory ($out/config) and configures it with the
# CLI: provider deepseek (key via stdin), route "sonnet" -> deepseek-flash.
#
#   e2e/run.sh            run all cases
#   e2e/run.sh A D        run selected cases
#
# Cases (spike numbering): A general-purpose + model "sonnet"; B1 agent with
# model: sonnet; B2 agent with model: claude-sonnet-5-5; C general-purpose
# without model (CLAUDE_CODE_SUBAGENT_MODEL=sonnet); D main thread on sonnet;
# E fork subagent; F multi-turn tool use inside a routed subagent;
# G interactive background session (claude --bg, settings mode) — interactive
# sessions carry extra tools (Artifact) that -p runs do not; H env mode via
# `shunt claude`. G runs in the repo root (must be a trusted workspace) and
# removes its background session afterwards.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
out=$root/e2e/out
port=${SHUNT_E2E_PORT:-18092}
: "${DEEPSEEK_API_KEY:?DEEPSEEK_API_KEY must be set}"

case $DEEPSEEK_API_KEY in *$'\n'*)
  echo "DEEPSEEK_API_KEY contains a newline" >&2; exit 2 ;;
esac

rm -rf "$out" && mkdir -p "$out/work" && install -d -m700 "$out/config"
go build -o "$out/shunt" "$root"

cat > "$out/settings.json" <<JSON
{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:$port",
         "ANTHROPIC_DEFAULT_SONNET_MODEL": "shunt/sonnet",
         "CLAUDE_CODE_SUBAGENT_MODEL": "sonnet"}}
JSON
cat > "$out/agents.json" <<'JSON'
{"fm-sonnet": {"description": "Arithmetic helper (alias sonnet)", "prompt": "Answer the arithmetic question; deliver only the number as your report.", "model": "sonnet"},
 "fm-fullid": {"description": "Arithmetic helper (full sonnet id)", "prompt": "Answer the arithmetic question; deliver only the number as your report.", "model": "claude-sonnet-5-5"}}
JSON

# Start from an empty config directory and configure it through the admin
# API, as a user would.
"$out/shunt" serve --config-dir "$out/config" -l "127.0.0.1:$port" 2> "$out/proxy.log" &
proxy_pid=$!
trap 'kill $proxy_pid 2>/dev/null || true' EXIT
admin=(--config-dir "$out/config" -u "http://127.0.0.1:$port")
for _ in $(seq 50); do
  "$out/shunt" provider list "${admin[@]}" > /dev/null 2>&1 && break
  sleep 0.1
done
"$out/shunt" provider add deepseek -b https://api.deepseek.com/anthropic "${admin[@]}"
# printf is a builtin: the key goes through stdin, never a process argument.
printf '%s\n' "$DEEPSEEK_API_KEY" | "$out/shunt" provider set-key deepseek "${admin[@]}"
"$out/shunt" route set sonnet deepseek deepseek-flash -s "$out/settings.json" "${admin[@]}"
"$out/shunt" provider list "${admin[@]}"

fails=0
pass() { echo "  PASS $*"; }
fail() { echo "  FAIL $*"; fails=$((fails + 1)); }

# run_case <id> <prompt> [claude args...]: runs claude -p, saves output and the
# proxy log lines produced during the run.
run_case() {
  local id=$1 prompt=$2; shift 2
  local start; start=$(wc -l < "$out/proxy.log")
  (cd "$out/work" && timeout 600 claude -p "$prompt" --settings "$out/settings.json" \
      --agents "$(cat "$out/agents.json")" --max-turns 8 "$@" > "$out/$id.out" 2>&1) || true
  segment "$id" "$start"
}
# segment <id> <start-line>: proxy log lines produced since start-line.
segment() {
  tail -n +"$(($2 + 1))" "$out/proxy.log" | grep 'msg=request' > "$out/$1.log" || true
  echo "case $1:"
}
has() { grep -q -- "$2" "$out/$1.log"; }

want() { case " ${CASES:-A B1 B2 C D E F G H} " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }
CASES="${*:-}"

if want A; then
  run_case A 'Use the Agent tool exactly once: subagent_type "general-purpose", model "sonnet", prompt "Compute 17*23 and deliver only the number as your report.". Then print the subagent report verbatim.'
  has A 'target=deepseek status=200' && pass "subagent routed to deepseek" || fail "no deepseek 200"
  grep -q 391 "$out/A.out" && pass "report reached main thread" || fail "main thread output lacks 391"
fi
if want B1; then
  run_case B1 'Use the Agent tool exactly once with subagent_type "fm-sonnet" and prompt "Compute 19*21.". Do not pass a model. Then print its report verbatim.'
  has B1 'target=deepseek status=200' && pass "model: sonnet agent -> deepseek" || fail "fm-sonnet not routed"
  grep -q 399 "$out/B1.out" && pass "report reached main thread" || fail "main thread output lacks 399"
fi
if want B2; then
  run_case B2 'Use the Agent tool exactly once with subagent_type "fm-fullid" and prompt "Compute 12*12.". Do not pass a model. Then print its report verbatim.'
  has B2 'model=claude-sonnet-5-5 target=anthropic' && pass "full-id agent -> anthropic" || fail "full-id agent did not hit anthropic"
  has B2 'target=deepseek' && fail "full-id case touched deepseek" || pass "deepseek untouched"
fi
if want C; then
  run_case C 'Use the Agent tool exactly once with subagent_type "general-purpose" and prompt "Compute 13*17 and deliver only the number as your report.". Do NOT pass a model. Then print its report verbatim.'
  has C 'target=deepseek status=200' && pass "general-purpose (no model) -> deepseek" || fail "general-purpose not routed"
fi
if want D; then
  run_case D 'Say hi in one word.' --model sonnet
  has D 'model=shunt/sonnet target=rejected status=400' && pass "main thread on sonnet rejected" || fail "main thread not rejected"
  grep -q 'main-thread' "$out/D.out" && pass "error message shown to user" || fail "error message missing from output"
  has D 'target=deepseek' && fail "main thread reached deepseek" || pass "deepseek untouched"
fi
if want F; then
  # Multi-turn tool use on DeepSeek: thinking/tool history must round-trip.
  echo "the code word is PERIWINKLE-7" > "$out/work/note.txt"
  run_case F 'Use the Agent tool exactly once with subagent_type "fm-sonnet" and prompt "Use the Read tool to read note.txt in the current directory, then deliver the code word as your report.". Then print its report verbatim.'
  [[ $(grep -c 'target=deepseek status=200' "$out/F.log") -ge 2 ]] && pass "multi-turn tool use on deepseek" || fail "fewer than 2 deepseek turns"
  grep -q 'PERIWINKLE-7' "$out/F.out" && pass "tool result reached main thread" || fail "main thread output lacks code word"
  has F 'target=deepseek status=[45]' && fail "deepseek returned an error" || pass "no deepseek errors"
fi
if want E; then
  run_case E 'Use the Agent tool exactly once with subagent_type "fork" and prompt "Reply with the word ok.". Then stop.'
  has E 'target=deepseek' && fail "fork reached deepseek" || pass "fork stayed on anthropic"
fi
if want G; then
  start=$(wc -l < "$out/proxy.log")
  name=shunt-e2e-G-$$
  (cd "$root" && timeout 60 claude --bg --name "$name" --settings "$out/settings.json" \
    'Use the Agent tool exactly once: subagent_type "general-purpose", model "sonnet", prompt "Compute 23*29 and deliver only the number as your report.". Do not read or edit any files. Wait for the agent, then print its report verbatim and stop.' \
    > "$out/G.launch" 2>&1) || true
  timeout 300 bash -c "until claude agents --json 2>/dev/null | jq -e '.[] | select(.name==\"$name\" and .status==\"idle\")' >/dev/null; do sleep 3; done" || true
  id=$(claude agents --json --all 2>/dev/null | jq -r ".[] | select(.name==\"$name\") | .id" | head -1)
  [[ -n $id ]] && timeout 30 claude logs "$id" 2>&1 | sed 's/\x1b\[[0-9;]*[A-Za-z]//g' > "$out/G.out" || true
  [[ -n $id ]] && { claude stop "$id" >/dev/null 2>&1; claude rm "$id" >/dev/null 2>&1; } || true
  segment G "$start"
  has G 'target=deepseek status=200' && pass "interactive bg subagent -> deepseek" || fail "no deepseek 200 from bg session"
  has G 'target=deepseek status=[45]' && fail "deepseek returned an error (interactive tool schemas?)" || pass "no deepseek errors"
  grep -q 667 "$out/G.out" && pass "report reached main thread" || fail "bg session output lacks 667"
fi
if want H; then
  start=$(wc -l < "$out/proxy.log")
  (cd "$out/work" && timeout 600 "$out/shunt" claude --router-url "http://127.0.0.1:$port" --config-dir "$out/config" -- -p \
    'Use the Agent tool exactly once: subagent_type "general-purpose", model "sonnet", prompt "Compute 31*37 and deliver only the number as your report.". Then print the report verbatim.' \
    --max-turns 6 > "$out/H.out" 2>&1) || true
  segment H "$start"
  has H 'model=claude-[a-z0-9-]* target=anthropic status=200' && pass "env mode: main thread -> anthropic" || fail "env mode main thread missing"
  has H 'target=deepseek status=200' && pass "env mode: subagent -> deepseek" || fail "env mode subagent not routed"
  grep -q 1147 "$out/H.out" && pass "report reached main thread" || fail "main thread output lacks 1147"
fi

grep -qi 'sk-ant-' "$out/proxy.log" && fail "proxy log contains sk-ant-" || pass "proxy log free of sk-ant-"
echo "logs and outputs: $out"
[[ $fails -eq 0 ]] && echo "e2e: ok" || { echo "e2e: $fails failure(s)"; exit 1; }
