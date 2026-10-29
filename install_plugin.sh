#!/usr/bin/env bash
# Install herdr-recall into Herdr's own plugin folder.
# Do not link a checkout under ~/code. That path can disappear, and prefix L
# then fails or opens an old copy.
#
# Transcript search is optional. It needs ccrider. Without ccrider, the
# Transcripts tab is hidden.
#
#   ./install_plugin.sh                  # asks if a terminal is attached
#   ./install_plugin.sh --with-transcripts
#   ./install_plugin.sh --no-transcripts
#   HERDR_RECALL_WITH_TRANSCRIPTS=1 ./install_plugin.sh main
set -euo pipefail

REF="main"
CHOICE="${HERDR_RECALL_WITH_TRANSCRIPTS:-}"
for arg in "$@"; do
  case "$arg" in
    --with-transcripts) CHOICE=1 ;;
    --no-transcripts) CHOICE=0 ;;
    -h|--help)
      echo "usage: install_plugin.sh [ref] [--with-transcripts|--no-transcripts]"
      exit 0
      ;;
    --*)
      echo "unknown flag: $arg" >&2
      exit 2
      ;;
    *) REF="$arg" ;;
  esac
done

want_transcripts() {
  case "$CHOICE" in
    1|y|Y|yes|true) return 0 ;;
    0|n|N|no|false) return 1 ;;
  esac
  if [[ ! -t 0 ]]; then
    return 1
  fi
  local ans
  read -r -p "Install optional transcript search (ccrider)? [y/N] " ans
  [[ "$ans" == "y" || "$ans" == "Y" || "$ans" == "yes" ]]
}

install_ccrider() {
  if command -v ccrider >/dev/null 2>&1; then
    echo "ccrider is already on PATH: $(command -v ccrider)"
    return 0
  fi
  if ! command -v go >/dev/null 2>&1; then
    echo "Go is not on PATH. Skipping ccrider. The Transcripts tab stays hidden." >&2
    return 0
  fi
  mkdir -p "${HOME}/.local/bin"
  GOBIN="${HOME}/.local/bin" go install github.com/neilberkman/ccrider/cmd/ccrider@latest
  echo "installed ccrider to ${HOME}/.local/bin/ccrider"
  echo "Run 'ccrider sync' once so the Transcripts tab has an index."
}

herdr plugin install RooseveltAdvisors/herdr-recall --ref "$REF" --yes
herdr server reload-config
echo "installed RooseveltAdvisors/herdr-recall@$REF into Herdr's plugin folder"

if want_transcripts; then
  install_ccrider
else
  echo "Skipped ccrider. The Transcripts tab stays hidden until ccrider is on PATH."
fi
