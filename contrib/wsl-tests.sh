#!/bin/bash
# Run every Go test package on Linux from a Windows checkout (Git Bash), in WSL, plus the launcher
# test. The code is Linux-only (GOOS=linux), so each package's test binary is cross-built here and
# run in WSL from a copy of the repository on WSL's own filesystem (permission tests misbehave on
# /mnt/c). With the Docker Jellyfin test server up (contrib/testserver), the live tests run too.
#   sh contrib/wsl-tests.sh [WSL distro]        (default Ubuntu-26.04)
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd -W 2>/dev/null || pwd)
DISTRO=${1:-Ubuntu-26.04}
BIN="$ROOT/target/testbin"
mkdir -p "$BIN"
cd "$ROOT"
pkgs=$(GOOS=linux go list ./... | sed "s#^$(go list -m)/##")
for p in $pkgs; do
  n=$(echo "$p" | tr '/' '_')
  GOOS=linux GOARCH=amd64 go test -c -o "$BIN/$n.test" "./$p" 2>/dev/null || true
done
wslpath() { echo "$1" | sed -E 's#^([A-Za-z]):#/mnt/\L\1#; s#\\#/#g'; }
WROOT=$(wslpath "$ROOT")
SERVER=$(wslpath "$ROOT/../jellyfin-test/server.json")
cat > "$BIN/run.sh" <<EOF
#!/bin/bash
if [ -f "$SERVER" ]; then export JELLYFIN_TEST_SERVER="$SERVER"; fi
rm -rf ~/qj-tests && cp -r "$WROOT" ~/qj-tests
fail=0
for t in "$WROOT"/target/testbin/*.test; do
  n=\$(basename "\$t" .test); d=\$(echo "\$n" | tr '_' '/')
  cp "\$t" ~/"\$n.test" && chmod +x ~/"\$n.test"
  if (cd ~/qj-tests/"\$d" && ~/"\$n.test" -test.timeout 300s > ~/"\$n.log" 2>&1); then echo "PASS \$d"
  else echo "FAIL \$d"; grep -A6 -E '^--- FAIL' ~/"\$n.log" | head -30; fail=1; fi
done
(cd ~/qj-tests/contrib/shanlingq2 && python3 test_launcher.py > ~/launcher.log 2>&1) && echo "PASS launcher" || { echo "FAIL launcher"; fail=1; }
exit \$fail
EOF
MSYS_NO_PATHCONV=1 wsl -d "$DISTRO" -- bash "$(wslpath "$BIN/run.sh")"
