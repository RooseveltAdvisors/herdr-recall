#!/usr/bin/env bash
# Install herdr-recall into Herdr's own plugin folder.
# Do not link a checkout under ~/code. That path can disappear, and prefix L
# then fails or opens an old copy.
set -euo pipefail

REF="${1:-main}"
herdr plugin install RooseveltAdvisors/herdr-recall --ref "$REF" --yes
herdr server reload-config
echo "installed RooseveltAdvisors/herdr-recall@$REF into Herdr's plugin folder"
