#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit -o nounset -o pipefail

printf '%s\n' "$*" >>"${RELEASE_TEST_LOG}"
if [[ "${RELEASE_TEST_FAIL:-}" == "$1" ]]; then
  echo "helm $1 failed" >&2
  exit 1
fi
case "$1" in
  package)
    chart="${2##*/}"
    shift 2
    destination=
    version=
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --destination) destination="$2" ;;
        --version) version="$2" ;;
        --app-version) ;;
        *) exit 2 ;;
      esac
      shift 2
    done
    touch "${destination}/${chart}-${version}.tgz"
    ;;
  registry) cat >/dev/null ;;
  push) test -f "$2" ;;
  *) exit 2 ;;
esac
