#!/usr/bin/env bash
# Table tests for the version and tag selection in xs-sync.sh and its small
# pure helpers. No network, no GitHub token. Run: .github/scripts/xs-sync-test.sh
# shellcheck disable=SC2016,SC2046,SC2086,SC2154,SC2310,SC2317,SC2329 # word splitting of tag lists, mock functions and names set by the sourced script are intended
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export XS_SYNC_SOURCE_ONLY=1
# shellcheck source=/dev/null
source "${here}/xs-sync.sh"
set +e

fail=0
pass=0

# expect <name> <want> <got>
expect() {
  if [[ "$2" == "$3" ]]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    printf 'FAIL %s\n  want: %s\n  got:  %s\n' "$1" "$2" "$3"
  fi
}

# sel <tags (space separated)> <current> [open heads...] -> "latest proceed supersede"
sel() {
  local tags="$1" current="$2"
  shift 2
  local r
  # shellcheck disable=SC2086 # the tag list is split on purpose
  r="$(printf '%s\n' ${tags} | xs_select "${current}" "$@" 2>&1)" || { echo "error: ${r}"; return; }
  printf '%s %s [%s]\n' "$(grep '^latest=' <<<"${r}" | cut -d= -f2)" \
    "$(grep '^proceed=' <<<"${r}" | cut -d= -f2)" "$(grep '^supersede=' <<<"${r}" | cut -d= -f2)"
}

loki="v2.9.20 v3.6.11 v3.6.12 v3.7.0 v3.7.8 v3.7.9 v3.8.0-rc.1 v3.8.0-rc.2 helm-loki-6.0.0 operator/v0.9.0 pkg/push/v0.1.0 v3.7.9-xs.0"

# nothing new
expect "nothing new" "v3.7.9 false []" "$(sel "${loki}" v3.7.9)"
# patch release
expect "patch" "v3.7.10 true []" "$(sel "${loki} v3.7.10" v3.7.9)"
# numeric, not lexical, ordering
expect "10 > 9" "v3.7.10 true []" "$(sel "v3.7.9 v3.7.10 v3.7.2" v3.7.9)"
# minor release
expect "minor" "v3.8.0 true []" "$(sel "${loki} v3.8.0" v3.7.9)"
# release candidates are ignored
expect "rc ignored" "v3.7.9 false []" "$(sel "${loki} v3.8.1-rc.1 v3.8.0-beta.1" v3.7.9)"
# a patch on an older release line does not move the target
expect "other line" "v3.7.9 false []" "$(sel "${loki} v3.6.13 v2.9.21" v3.7.9)"
# newest stable wins even when several are new
expect "skip to newest" "v3.7.11 true []" "$(sel "${loki} v3.7.10 v3.7.11" v3.7.9)"
# our own release tags are not upstream tags
expect "xs tags ignored" "v3.7.9 false []" "$(sel "${loki} v3.7.10-xs.0 v3.8.0-xs.1" v3.7.9)"
# already open PR for the target
expect "pr open" "v3.7.10 false []" "$(sel "${loki} v3.7.10" v3.7.9 sync/v3.7.10)"
# older sync PRs are superseded, the one for the target is kept
expect "supersede" "v3.7.11 true [sync/v3.7.10]" "$(sel "${loki} v3.7.10 v3.7.11" v3.7.9 sync/v3.7.10)"
expect "supersede, pr open" "v3.7.11 false [sync/v3.7.10]" "$(sel "${loki} v3.7.10 v3.7.11" v3.7.9 sync/v3.7.10 sync/v3.7.11)"
# stale PR after main caught up is still superseded
expect "stale pr" "v3.7.10 false [sync/v3.7.9]" "$(sel "${loki} v3.7.10" v3.7.10 sync/v3.7.9)"
# unrelated PR heads are ignored
expect "other heads" "v3.7.10 true []" "$(sel "${loki} v3.7.10" v3.7.9 feature/x dependabot/foo)"
# cap
expect "cap" "v3.7.9 true []" "$(XS_CAP_TAG=v3.7.9 sel "${loki} v3.8.0" v3.7.8)"
# bad current
expect "bad current" "error: ::error::current tag v3.7.9-xs.0 is not a stable release tag" "$(sel "${loki}" v3.7.9-xs.0)"
# highest stable helper, no stable tag
expect "highest" "v3.7.9" "$(printf '%s\n' "${loki}" | tr ' ' '\n' | xs_highest_stable)"
expect "highest none" "" "$(printf '%s\n' v3.8.0-rc.1 foo | xs_highest_stable)"

# mimir naming
(
  # shellcheck disable=SC2034 # read by the sourced functions
  XS_TAG_REGEX='^mimir-([0-9]+)\.([0-9]+)\.([0-9]+)$'
  # shellcheck disable=SC2034
  XS_TAG_FORMAT='mimir-%s'
  mimir="mimir-2.17.0 mimir-3.2.2 mimir-3.2.3 mimir-3.3.0-rc.0 mimir-3.3.0-rc.1 mimir-2.10.5 v3.9.0 mimir-distributed-5.0.0 operator/v1"
  expect "mimir nothing new" "mimir-3.2.3 false []" "$(sel "${mimir}" mimir-3.2.3)"
  expect "mimir patch" "mimir-3.2.4 true []" "$(sel "${mimir} mimir-3.2.4" mimir-3.2.3)"
  expect "mimir minor, rc ignored" "mimir-3.3.0 true []" "$(sel "${mimir} mimir-3.3.0" mimir-3.2.3)"
  expect "mimir rc only" "mimir-3.2.3 false []" "$(sel "${mimir}" mimir-3.2.3)"
  expect "mimir tag name" "mimir-3.3.0" "$(xs_tag_of 3.3.0)"
  expect "mimir version" "3.2.3" "$(xs_version_of mimir-3.2.3)"
  expect "mimir foreign tag" "" "$(xs_version_of v3.2.3)"
)

# version and tag helpers
expect "version" "3.7.9" "$(xs_version_of v3.7.9)"
expect "version rc" "" "$(xs_version_of v3.7.9-rc.1)"
expect "version newline" "" "$(xs_version_of $'v3.7.9\nx')"
expect "tag of" "v3.7.9" "$(xs_tag_of 3.7.9)"
xs_ver_gt 3.7.10 3.7.9 && r=yes || r=no
expect "gt 3.7.10 3.7.9" "yes" "${r}"
xs_ver_gt 3.7.9 3.7.9 && r=yes || r=no
expect "gt equal" "no" "${r}"
xs_ver_gt 3.10.0 3.9.9 && r=yes || r=no
expect "gt minor numeric" "yes" "${r}"
xs_ver_gt 2.9.20 3.0.0 && r=yes || r=no
expect "gt major" "no" "${r}"

# patch path rules
for p in .github/workflows/xs-test.yml .github/scripts/xs-sync.sh go.mod docs/sources/tempo/configuration/manifest.md modules/frontend/docs/config-reference.md modules/overrides/adminstore_bucket.go vendor/modules.txt vendor/github.com/xscaler/dskit/xstenant/source.go; do
  xs_path_allowed "${p}" && r=yes || r=no
  expect "allowed ${p}" "yes" "${r}"
done
for p in .github/workflows/check.yml .github/scripts/other.sh pkg/loki/loki.go modules/overrides/interface.go vendor/github.com/other/x.go vendor/github.com/xscalerx/y.go; do
  xs_path_allowed "${p}" && r=yes || r=no
  expect "not allowed ${p}" "no" "${r}"
done

# flag names from help output
tmp="$(mktemp -d)"
printf 'Usage of x:\n  -a.b\n    \tdoc -not-a-flag\n  -c value\n    \tdoc\n  -a.b\n' > "${tmp}/h"
expect "flag names" "$(printf -- '-a.b\n-c')" "$(flag_names "${tmp}/h")"
rm -rf "${tmp}"

# body fill
tmp="$(mktemp -d)"
printf 'head\n%s\nPending\n%s\ntail\n' "${XS_FLAG_BEGIN}" "${XS_FLAG_END}" > "${tmp}/body"
printf 'line 1\nline 2\n' > "${tmp}/fill"
expect "body fill" "$(printf 'head\n%s\nline 1\nline 2\n%s\ntail' "${XS_FLAG_BEGIN}" "${XS_FLAG_END}")" "$(xs_body_fill "${tmp}/body" "${tmp}/fill")"
xs_body_fill "${tmp}/body" "${tmp}/fill" > "${tmp}/body2"
printf 'new\n' > "${tmp}/fill2"
expect "body refill" "$(printf 'head\n%s\nnew\n%s\ntail' "${XS_FLAG_BEGIN}" "${XS_FLAG_END}")" "$(xs_body_fill "${tmp}/body2" "${tmp}/fill2")"
rm -rf "${tmp}"

# ---------------------------------------------------------------------------
# Helpers that need no repository
# ---------------------------------------------------------------------------
expect "own heads" "$(printf 'sync/v3.7.10\nfeature/x')" "$(printf '%s' '[{"headRefName":"sync/v3.7.10","isCrossRepository":false},{"headRefName":"sync/v9.9.9","isCrossRepository":true},{"headRefName":"feature/x","isCrossRepository":false}]' | xs_own_heads)"
# a fork PR named like ours is not counted: nothing superseded, nothing open
expect "fork pr ignored" "v3.7.10 true []" "$(sel "${loki} v3.7.10" v3.7.9 $(printf '%s' '[{"headRefName":"sync/v3.7.10","isCrossRepository":true}]' | xs_own_heads))"
# hostile head names are not treated as sync heads
expect "odd head" "v3.7.10 true []" "$(sel "${loki} v3.7.10" v3.7.9 'sync/v3.7.9;rm' 'sync/')"

expect "prev same line" "v3.7.8" "$(printf '%s\n' ${loki} | tr ' ' '\n' | xs_prev_tag v3.7.9)"
expect "prev across minor" "v3.6.12" "$(printf '%s\n' ${loki} | tr ' ' '\n' | xs_prev_tag v3.7.0)"

# tempo naming: v<x.y.z> tags, release-v<x.y> branches
tempo="v2.10.0 v2.10.0-rc.0 v3.0.5 v3.1.0-rc.1 v3.1.0 v3.1.1 v3.1.1-xs.0"
expect "tempo nothing new" "v3.1.1 false []" "$(sel "${tempo}" v3.1.1)"
expect "tempo patch" "v3.1.2 true []" "$(sel "${tempo} v3.1.2" v3.1.1)"
expect "tempo minor, rc ignored" "v3.2.0 true []" "$(sel "${tempo} v3.2.0 v3.3.0-rc.0" v3.1.1)"
expect "tempo prev same line" "v3.1.0" "$(printf '%s\n' ${tempo} | tr ' ' '\n' | xs_prev_tag v3.1.1)"
for b in main release-v3.1 release-v2.10 release-v1.3.0; do
  [[ "${b}" =~ ${XS_BRANCH_REGEX} ]] && r=yes || r=no
  expect "tempo mirrors ${b}" "yes" "${r}"
done
for b in backport-7427-to-release-v3.0 renovate/release-v2.10-docker release-v3.1-foo upstream/main; do
  [[ "${b}" =~ ${XS_BRANCH_REGEX} ]] && r=yes || r=no
  expect "tempo skips ${b}" "no" "${r}"
done

expect "md paths" "$(printf -- "- \`a b\`\n- \`x'y\`\n- \`l1?l2\`")" "$(printf 'a b\0x`y\0l1\nl2\0' | xs_md_paths)"
expect "plain paths" "- @types/x" "$(printf '@types/x\0' | xs_md_paths plain)"

# retry with backoff on throttling, no retry on other errors
cf="$(mktemp)"
echo 0 > "${cf}"
gh() {
  local calls
  calls=$(($(cat "${cf}") + 1))
  echo "${calls}" > "${cf}"
  if ((calls <= 2)); then echo "gh: HTTP 429 You have exceeded a secondary rate limit" >&2; return 1; fi
  echo '{"sha":"abc"}'
}
echo '{}' > /tmp/xs-test-empty.json
XS_RETRY_SLEEP=0 api_call POST repos/x/y/git/blobs /tmp/xs-test-empty.json > /tmp/xs-test-out 2>&1
expect "retry throttled" "3 {\"sha\":\"abc\"}" "$(cat "${cf}") $(tail -n1 /tmp/xs-test-out)"
echo 0 > "${cf}"
gh() { echo $(($(cat "${cf}") + 1)) > "${cf}"; echo "gh: Not Found (HTTP 404)" >&2; return 1; }
XS_RETRY_SLEEP=0 api_call POST repos/x/y/git/blobs /tmp/xs-test-empty.json > /dev/null 2>&1
rc=$?
expect "no retry on 404" "1 1" "$(cat "${cf}") ${rc}"
unset -f gh
rm -f /tmp/xs-test-empty.json /tmp/xs-test-out "${cf}"

# S2: only heads the bot creates, and only lower versions, are superseded
expect "human branch kept" "v3.7.10 true []" "$(sel "${loki} v3.7.10" v3.7.9 sync/fix-conflict sync/v3.7.10-hotfix sync/my-branch)"
expect "higher head kept" "v3.7.10 true []" "$(sel "${loki} v3.7.10" v3.7.9 sync/v3.9.0)"
expect "lower bot head superseded" "v3.7.10 true [sync/v3.7.8]" "$(sel "${loki} v3.7.10" v3.7.9 sync/v3.7.8 sync/fix-x)"
# S3 input: an open PR for the target is reported so the sync can refresh it
r="$(printf '%s\n' ${loki} v3.7.10 | xs_select v3.7.9 sync/v3.7.10)"
expect "open_pr flag" "open_pr=true" "$(grep '^open_pr=' <<<"${r}")"
expect "no open_pr flag" "open_pr=false" "$(printf '%s\n' ${loki} v3.7.10 | xs_select v3.7.9 | grep '^open_pr=')"

# S1: PR state of a head, own PRs only
st() { printf '%s' "$1" | xs_pr_state; }
expect "pr none" "none" "$(st '[]')"
expect "pr closed" "closed 7" "$(st '[{"number":7,"state":"CLOSED","isCrossRepository":false}]')"
expect "pr merged" "merged 8" "$(st '[{"number":7,"state":"CLOSED","isCrossRepository":false},{"number":8,"state":"MERGED","isCrossRepository":false}]')"
expect "pr open wins" "open 9" "$(st '[{"number":7,"state":"CLOSED","isCrossRepository":false},{"number":9,"state":"OPEN","isCrossRepository":false}]')"
expect "pr fork ignored" "none" "$(st '[{"number":7,"state":"OPEN","isCrossRepository":true}]')"

# S4b: which workflows get disabled
wf="$(printf '1\t.github/workflows/xs-test.yml\tactive\n2\t.github/workflows/check.yml\tactive\n3\t.github/workflows/old.yml\tdisabled_manually\n4\t.github/workflows/images.yaml\tactive\n5\tdynamic/dependabot\tactive\n6\t.github/workflows/xs-.yml.evil/x.yml\tactive\n' | xs_workflows_to_disable | cut -f1 | tr '\n' ' ')"
expect "workflows to disable" "2 4 6 " "${wf}"

# S5: neutralise upstream text
expect "neutralize" "$(printf 'a@\xe2\x80\x8bb \xe2\x80\xb9x\xe2\x80\xba %sy' "'")" "$(printf 'a@b <x> ``y' | xs_neutralize)"
expect "neutralize fence" "no" "$(printf 'x ``` y ````` z\n' | xs_neutralize | grep -q '`' && echo yes || echo no)"

# ---------------------------------------------------------------------------
# Scratch repositories
# ---------------------------------------------------------------------------
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT

# in <repo> <command...>: run in the repository, output and status in OUT and RC.
in_repo() {
  local d="$1"
  shift
  OUT="$(cd "${d}" && "$@" 2>&1)"
  RC=$?
}
G() { git -C "${repo}" "$@"; }

# Two syncs: main = upstream tag + patch, then merge commits for v1.0.1 and v1.0.2.
repo="${scratch}/sync"
git init -q -b up "${repo}"
printf 'one\ntwo\nthree\n' > "${repo}/a.txt"
G add a.txt && G commit -q -m u0 && G tag v1.0.0
G checkout -q -b main
mkdir -p "${repo}/.github/workflows"
echo patch > "${repo}/.github/workflows/xs-x.yml"
G add . && G commit -q -m patch
G checkout -q up
printf 'one\ntwo\nthree\nfour\n' > "${repo}/a.txt" && G commit -q -am u1 && G tag v1.0.1
G tag v1.0.1-rc.1
G checkout -q main && G merge -q --no-ff -m "sync v1.0.1" v1.0.1
in_repo "${repo}" cmd_detect --base main
expect "detect after one sync" "current=v1.0.1 latest=v1.0.1 proceed=false" "$(grep -E '^(current|latest|proceed)=' <<<"${OUT}" | tr '\n' ' ' | sed 's/ $//')"
G checkout -q up
printf 'one\ntwo\nthree\nfour\nfive\n' > "${repo}/a.txt" && G commit -q -am u2 && G tag v1.0.2
G checkout -q main
in_repo "${repo}" cmd_detect --base main
expect "detect new patch after one sync" "current=v1.0.1 latest=v1.0.2 proceed=true" "$(grep -E '^(current|latest|proceed)=' <<<"${OUT}" | tr '\n' ' ' | sed 's/ $//')"
G merge -q --no-ff -m "sync v1.0.2" v1.0.2
in_repo "${repo}" cmd_detect --base main
expect "detect after two syncs" "current=v1.0.2 latest=v1.0.2 proceed=false" "$(grep -E '^(current|latest|proceed)=' <<<"${OUT}" | tr '\n' ' ' | sed 's/ $//')"
# a sync PR for the newest release is already open
G checkout -q up
printf 'one\ntwo\nthree\nfour\nfive\nsix\n' > "${repo}/a.txt" && G commit -q -am u3 && G tag v1.0.3
G checkout -q main
printf 'sync/v1.0.3\n' > "${scratch}/heads"
in_repo "${repo}" cmd_detect --base main --open-heads "${scratch}/heads"
expect "detect with open pr" "latest=v1.0.3 proceed=false" "$(grep -E '^(latest|proceed)=' <<<"${OUT}" | tr '\n' ' ' | sed 's/ $//')"
# the patch set is only our file, however many merges main has
expect "patch files after merges" ".github/workflows/xs-x.yml" "$(cd "${repo}" && patch_files v1.0.2 main | tr '\0' '\n')"

# Squash merge of v1.0.3: the content is there, the tag is not in the history.
G checkout -q -b squashed main
G checkout v1.0.3 -- a.txt
G commit -q -m "squash v1.0.3"
in_repo "${repo}" cmd_detect --base squashed
expect "squash detected" "1 yes" "${RC} $(grep -q 'already holds the content of v1.0.3' <<<"${OUT}" && echo yes || echo no)"
G checkout -q main

# Identity check with a rename: the old path must not hide behind the new one.
G checkout -q -b rename v1.0.3
G mv a.txt b.txt && G commit -q -m rename
in_repo "${repo}" cmd_check identity v1.0.3 rename
expect "rename fails identity" "1 yes yes" "${RC} $(grep -q 'a.txt differs' <<<"${OUT}" && echo yes || echo no) $(grep -q 'b.txt differs' <<<"${OUT}" && echo yes || echo no)"
G checkout -q -b okpatch v1.0.3
mkdir -p "${repo}/.github/workflows" && echo p > "${repo}/.github/workflows/xs-y.yml"
G add . && G commit -q -m ok
in_repo "${repo}" cmd_check identity v1.0.3 okpatch
expect "patch path passes identity" "0" "${RC}"
# version check: v1.0.2 sync commit that has v1.0.3 in its history is rejected
in_repo "${repo}" cmd_check version v1.0.2 okpatch
expect "newer tag in history fails version" "1" "${RC}"
in_repo "${repo}" cmd_check version v1.0.3 okpatch
expect "version ok" "0" "${RC}"

# S4a: upstream .github changes between two bases, ours excluded
G checkout -q -b gh1 v1.0.0
mkdir -p "${repo}/.github/workflows" "${repo}/.github/scripts"
echo a > "${repo}/.github/workflows/up.yml"; echo b > "${repo}/.github/workflows/del.yml"; echo x > "${repo}/.github/workflows/xs-own.yml"
echo c > "${repo}/CHANGELOG"
G add -A && G commit -q -m gh1 && G tag gh-a
echo a2 > "${repo}/.github/workflows/up.yml"; G rm -q .github/workflows/del.yml; echo n > "${repo}/.github/workflows/new.yml"; echo x2 > "${repo}/.github/workflows/xs-own.yml"; echo c2 > "${repo}/CHANGELOG"
G add -A && G commit -q -m gh2 && G tag gh-b
expect "github changes" "$(printf 'added .github/workflows/new.yml\ndeleted .github/workflows/del.yml\nmodified .github/workflows/up.yml')" "$(cd "${repo}" && xs_github_changes gh-a gh-b | LC_ALL=C sort)"
G checkout -q main

# S3: the first parent must be the current main head
G checkout -q -b s3 v1.0.3
G update-index --refresh -q
oldmain="$(G rev-parse main)"
G update-ref refs/remotes/origin/main "${oldmain}"
G checkout -q -b s3old "${oldmain}"
G merge -q --no-ff -m "sync v1.0.3" v1.0.3 2> /dev/null || { G checkout -q --theirs . 2> /dev/null; G add -A; G commit -q -m "sync v1.0.3"; }
in_repo "${repo}" cmd_check parents v1.0.3 s3old
expect "parents current main" "0" "${RC}"
G checkout -q main && printf 'x\n' > "${repo}/moved.txt" && G add moved.txt && G commit -q -m "main moved"
G update-ref refs/remotes/origin/main "$(G rev-parse main)"
in_repo "${repo}" cmd_check parents v1.0.3 s3old
expect "parents stale main" "1" "${RC}"

# ---------------------------------------------------------------------------
# xs namespace: upstream never owns it, a sync result never changes it
# ---------------------------------------------------------------------------
ns_yes() { if xs_ns_path "$1"; then echo yes; else echo no; fi; }
expect "ns workflow" "yes" "$(ns_yes .github/workflows/xs-evil.yml)"
expect "ns script" "yes" "$(ns_yes .github/scripts/xs-x.sh)"
expect "ns nested" "yes" "$(ns_yes .github/scripts/xs-x/y.sh)"
expect "ns upstream workflow" "no" "$(ns_yes .github/workflows/ci.yml)"
expect "ns lookalike" "no" "$(ns_yes .github/workflows/not-xs-x.yml)"
expect "ns outside .github" "no" "$(ns_yes docs/xs-x.md)"

repo="${scratch}/ns"
git init -q -b main "${repo}"
printf 'one\ntwo\nthree\n' > "${repo}/a.txt"
mkdir -p "${repo}/.github/workflows"
echo ci > "${repo}/.github/workflows/ci.yml"
G add -A && G commit -q -m base && G tag v1.0.0
base_tag_commit="$(G rev-parse HEAD)"
echo ours > "${repo}/.github/workflows/xs-x.yml"
G add -A && G commit -q -m patch
main_tree="$(G rev-parse 'HEAD^{tree}')"
G update-ref refs/heads/mainline HEAD
# v1.0.1: upstream adds an xs-* workflow of its own
G checkout -q -b u1 "${base_tag_commit}"
echo evil > "${repo}/.github/workflows/xs-evil.yml"
printf 'one\ntwo\nthree\nfour\n' > "${repo}/a.txt"
G add -A && G commit -q -m u1 && G tag v1.0.1
# v1.0.2: upstream rewrites one of our files (path matches xs-*)
G checkout -q -b u2 "${base_tag_commit}"
echo theirs > "${repo}/.github/workflows/xs-x.yml"
G add -A && G commit -q -m u2 && G tag v1.0.2
# v1.0.3: a clean upstream release
G checkout -q -b u3 "${base_tag_commit}"
printf 'one\ntwo\nthree\nfive\n' > "${repo}/a.txt"
G add -A && G commit -q -m u3 && G tag v1.0.3
# result = v1.0.3 plus our patch
result_tree() { # <tag> <xs-x content>
  G checkout -q --detach "$1"
  echo "$2" > "${repo}/.github/workflows/xs-x.yml"
  G add -A && G commit -q -m result
  G rev-parse 'HEAD^{tree}'
}
in_repo "${repo}" xs_ns_violations v1.0.3 "${main_tree}" "$(result_tree v1.0.3 ours)"
expect "ns clean sync" "0 " "${RC} ${OUT}"
in_repo "${repo}" xs_ns_violations v1.0.1
expect "ns upstream adds xs file" "0 upstream v1.0.1 contains .github/workflows/xs-evil.yml" "${RC} ${OUT}"
in_repo "${repo}" xs_ns_violations v1.0.2
expect "ns upstream owns one of ours" "0 upstream v1.0.2 contains .github/workflows/xs-x.yml" "${RC} ${OUT}"
in_repo "${repo}" xs_ns_violations v1.0.3 "${main_tree}" "$(result_tree v1.0.3 changed)"
expect "ns result differs from main" "0 result differs from main at .github/workflows/xs-x.yml" "${RC} ${OUT}"
in_repo "${repo}" xs_ns_violations no-such-ref
expect "ns git failure is an error" "1" "${RC}"
# a path with a newline cannot forge a log line
G checkout -q --detach v1.0.3
printf 'x\n' > "${repo}/.github/workflows/xs-$(printf 'a\nb').yml"
G add -A && G commit -q -m nl && G tag v1.0.4
in_repo "${repo}" xs_ns_violations v1.0.4
expect "ns path is sanitised" "1" "$(grep -c '' <<<"${OUT}")"
G checkout -q main

# identity check: a tag that holds xs files fails, even when the result keeps them
G checkout -q -b idns v1.0.1
echo ours > "${repo}/.github/workflows/xs-x.yml"; G add -A && G commit -q -m ours
in_repo "${repo}" cmd_check identity v1.0.1 idns
expect "identity rejects upstream xs path" "1 yes" "${RC} $(grep -q 'xs-evil.yml' <<<"${OUT}" && echo yes || echo no)"
G checkout -q -b idns3 v1.0.3
echo ours > "${repo}/.github/workflows/xs-x.yml"; G add -A && G commit -q -m ours
in_repo "${repo}" cmd_check identity v1.0.3 idns3
expect "identity accepts a clean tag" "0" "${RC}"
G checkout -q main

# workflows to disable: an xs-* workflow upstream also has is not ours
wfl="$(printf '1\t.github/workflows/xs-x.yml\tactive\n2\t.github/workflows/xs-evil.yml\tactive\n3\t.github/workflows/ci.yml\tactive\n')"
wf_with() { (cd "${repo}" && XS_UPSTREAM_REFS="$1" xs_workflows_to_disable <<<"${wfl}" | cut -f1 | tr '\n' ' '); }
expect "disable: no upstream refs" "3 " "$(wf_with '')"
expect "disable: upstream tag with xs file" "2 3 " "$(wf_with 'v1.0.1')"
expect "disable: clean upstream tag" "3 " "$(wf_with 'v1.0.3')"
expect "disable: upstream owns one of ours" "1 3 " "$(wf_with 'v1.0.2')"
expect "disable: any ref counts" "2 3 " "$(wf_with 'v1.0.3 v1.0.1')"

# mirror: upstream main (or a new tag) with xs paths is mirrored, but fails the run
mirror_check() { # <upstream main branch dir content tag> -> rc
  local mrepo="${scratch}/mirror-$1"
  git init -q --bare -b main "${mrepo}"
  git -C "${mrepo}" fetch -q "${repo}" "refs/tags/$1:refs/heads/main"
  local clone="${scratch}/mirror-clone-$1"
  git init -q -b work "${clone}"
  git -C "${clone}" remote add "${XS_UPSTREAM_REMOTE}" "${mrepo}"
  (cd "${clone}" && XS_RETRY_SLEEP=0 mirror_ns_check "${@:2}") 2>&1
  echo "rc=$?"
}
out="$(mirror_check v1.0.1)"
expect "mirror check: upstream main with xs path" "yes 1" "$(grep -q 'xs-evil.yml' <<<"${out}" && echo yes) $(grep -c '^rc=1' <<<"${out}")"
out="$(mirror_check v1.0.3)"
expect "mirror check: clean upstream main" "rc=0" "$(grep '^rc=' <<<"${out}")"
# a new tag with xs paths fails too
clone="${scratch}/mirror-clone-v1.0.3"
git -C "${clone}" fetch -q --no-tags "${scratch}/ns" refs/tags/v1.0.1:refs/tags/v1.0.1
out="$(cd "${clone}" && XS_RETRY_SLEEP=0 mirror_ns_check refs/tags/v1.0.1 2>&1; echo "rc=$?")"
expect "mirror check: new tag with xs path" "yes 1" "$(grep -q 'tags/v1.0.1' <<<"${out}" && echo yes) $(grep -c '^rc=1' <<<"${out}")"

# issue lookups only trust issues of the bot, with our label
calls="${scratch}/gh-calls"
gh() { printf '%s\n' "$*" >> "${calls}"; }
: > "${calls}"
issue_find "sync: v1.0.1 conflicts" sync-conflict > /dev/null
expect "issue_find author and label" "yes yes" "$(grep -q -e '--author app/xscaler-gremlin' "${calls}" && echo yes) $(grep -q -e '--label sync-conflict' "${calls}" && echo yes)"
: > "${calls}"
issue_find "sync: failed" sync-failed "${XS_ISSUE_AUTHOR_ACTIONS}" > /dev/null
expect "issue_find failure issue author" "yes" "$(grep -q -e '--author app/github-actions' "${calls}" && echo yes)"
in_repo "${repo}" issue_find "sync: x" ""
expect "issue_find needs a label" "1" "${RC}"
: > "${calls}"
cmd_issue_failed https://github.com/xscaler/loki/actions/runs/1 > /dev/null 2>&1
expect "issue-failed looks up with the Actions author" "yes yes" "$(grep -q -e '--author app/github-actions' "${calls}" && echo yes) $(grep -q -e '--label sync-failed' "${calls}" && echo yes)"
: > "${calls}"
rehearsal_cleanup > /dev/null 2>&1
REHEARSAL_TITLE="rehearsal sync: x" rehearsal_cleanup > /dev/null 2>&1
expect "rehearsal cleanup looks up by author and label" "yes yes" "$(grep -q -e '--author app/xscaler-gremlin' "${calls}" && echo yes) $(grep -q -e '--label rehearsal' "${calls}" && echo yes)"
unset -f gh

# api_plan: modify, delete, rename, mode change, symlink, binary.
repo="${scratch}/plan"
git init -q -b main "${repo}"
printf 'a\nb\nc\nd\ne\nf\n' > "${repo}/mod.txt"
printf 'x\ny\nz\n1\n2\n3\n' > "${repo}/ren.txt"
echo gone > "${repo}/del.txt"
echo '#!/bin/sh' > "${repo}/mode.sh"
G add . && G commit -q -m base
printf 'a\nB\nc\nd\ne\nf\n' > "${repo}/mod.txt"
G mv ren.txt ren2.txt
G rm -q del.txt
chmod +x "${repo}/mode.sh"
ln -s mod.txt "${repo}/ln"
printf '\000\001\002\377' > "${repo}/bin.dat"
G add -A && G commit -q -m head
in_repo "${repo}" api_plan "HEAD~1^{tree}" "HEAD^{tree}"
q() { jq -r "select(.path == \"$1\") | $2" <<<"${OUT}"; }
expect "plan rc" "0" "${RC}"
expect "plan modify" "100644 true" "$(q mod.txt '"\(.mode) \(.blob)"')"
expect "plan delete" "null" "$(q del.txt '.sha')"
expect "plan rename old" "null" "$(q ren.txt '.sha')"
expect "plan rename new" "100644 true" "$(q ren2.txt '"\(.mode) \(.blob)"')"
expect "plan mode only" "100755 false" "$(q mode.sh '"\(.mode) \(.blob)"')"
expect "plan symlink" "120000 true" "$(q ln '"\(.mode) \(.blob)"')"
expect "plan binary" "100644 true" "$(q bin.dat '"\(.mode) \(.blob)"')"
expect "plan entries" "7" "$(jq -s length <<<"${OUT}")"
# type change: file to symlink
G checkout -q -b typechg HEAD
rm "${repo}/mod.txt" && ln -s ln "${repo}/mod.txt" && G add -A && G commit -q -m typechg
in_repo "${repo}" api_plan "HEAD~1^{tree}" "HEAD^{tree}"
expect "plan type change" "120000 true" "$(q mod.txt '"\(.mode) \(.blob)"')"

# file to directory, directory to file, submodule
G checkout -q -b f2d main
rm "${repo}/del.txt" 2> /dev/null; echo f > "${repo}/swap" && G add -A && G commit -q -m swapfile
rm "${repo}/swap" && mkdir "${repo}/swap" && echo f > "${repo}/swap/inner" && G add -A && G commit -q -m swapdir
in_repo "${repo}" api_plan "HEAD~1^{tree}" "HEAD^{tree}"
expect "file to dir fails" "1 yes" "${RC} $(grep -q 'file to directory' <<<"${OUT}" && echo yes || echo no)"
in_repo "${repo}" api_plan "HEAD^{tree}" "HEAD~1^{tree}"
expect "dir to file fails" "1 yes" "${RC} $(grep -q 'directory to file' <<<"${OUT}" && echo yes || echo no)"
G checkout -q -b sub main
G update-index --add --cacheinfo "160000,$(G rev-parse HEAD),vendored" && G commit -q -m submodule
in_repo "${repo}" api_plan "HEAD~1^{tree}" "HEAD^{tree}"
expect "submodule fails" "1 yes" "${RC} $(grep -q 'submodule' <<<"${OUT}" && echo yes || echo no)"

echo "pass=${pass} fail=${fail}"
[[ "${fail}" -eq 0 ]]
