#!/usr/bin/env bash
# Link the DeepSeek Harness runtime packages into node_modules so the tests and
# esbuild can resolve @deepseek-ai/* (the dsh packages are private/not on npm).
# Run after `npm install`; `npm install` prunes extraneous symlinks, so re-run
# this whenever you reinstall.
set -euo pipefail
cd "$(dirname "$0")/.."

DSH="$(cd "${DSH_CHECKOUT:-../../../deepseek-harness}" && pwd)"
mkdir -p node_modules/@deepseek-ai
link() { ln -sfn "$DSH/$1" "node_modules/@deepseek-ai/$2"; }

link vendor/cordis                        cordis
link vendor/cosmokit                      cosmokit
link vendor/schemastery                   schemastery
link packages/subprocess/subprocess       dsh-subprocess
link packages/fs/fs                       dsh-fs
link packages/llm/llm                     dsh-llm
link packages/util/timeout                dsh-timeout
link packages/runtime-diagnostics/invariants dsh-invariants
link packages/util/brand                  dsh-brand

echo "linked dsh runtime packages from $DSH"
