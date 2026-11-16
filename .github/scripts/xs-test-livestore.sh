#!/usr/bin/env bash
# Runs the modules/livestore unit tests with one process per top-level test.
#
# With -race the package does not fit the 16 GB hosted runner in a single
# process: the race detector's shadow memory is not Go heap, so it grows with
# every test and GOMEMLIMIT cannot bound it. On Linux the package passes 7 GB
# in one process. One process per test bounds memory by the largest test.
#
# TestCutIdleTracesRespectsMaxBlockBytes builds 500,000 spans and passes 7 GB
# on its own with -race (5 GB without), so it runs without -race.
#
# usage: xs-test-livestore.sh <extra go test flags from the Makefile's GOTEST_OPT>
set -euo pipefail

heavy_test=TestCutIdleTracesRespectsMaxBlockBytes
pkgs=()
while IFS= read -r dir; do pkgs+=("$dir"); done < <(go list -f '{{.Dir}}' ./modules/livestore/...)
[ ${#pkgs[@]} -gt 0 ]
bin_dir=$(mktemp -d)
trap 'rm -rf "$bin_dir"' EXIT

# -v and -count are per run below; keep the Makefile's other flags (-race,
# -timeout) for the build and the runs.
build_flags=()
run_timeout=25m
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    -v | -count=*) ;;
    -timeout) run_timeout=${args[i + 1]}; i=$((i + 1)) ;;
    -timeout=*) run_timeout=${args[i]#-timeout=} ;;
    *) build_flags+=("${args[i]}") ;;
  esac
done

failed=()
for pkg in "${pkgs[@]}"; do
  name=$(echo "${pkg#"$PWD"/}" | tr / -)
  go test "${build_flags[@]}" -c -o "$bin_dir/$name.test" "$pkg"
  if [ ! -x "$bin_dir/$name.test" ]; then
    echo "no tests in $pkg"
    continue
  fi
  tests=$(cd "$pkg" && "$bin_dir/$name.test" -test.list '.*' | grep -E '^(Test|Example|Fuzz)' || true)
  for t in $tests; do
    [ "$t" = "$heavy_test" ] && continue
    echo "=== $pkg $t"
    if ! (cd "$pkg" && "$bin_dir/$name.test" -test.run "^${t}\$" -test.count=1 -test.v -test.timeout "$run_timeout"); then
      failed+=("$pkg $t")
    fi
  done
done

echo "=== ./modules/livestore/ $heavy_test (without -race)"
if ! go test -count=1 -v -timeout "$run_timeout" -run "^${heavy_test}\$" ./modules/livestore/; then
  failed+=("./modules/livestore/ $heavy_test")
fi

if [ ${#failed[@]} -gt 0 ]; then
  printf 'FAIL %s\n' "${failed[@]}"
  exit 1
fi
echo "PASS all modules/livestore tests"
