#!/usr/bin/env bash
# The app the agent starts from: a copy of skills/evals/lms-starter.
set -eu
cp -R "$(dirname "$0")/../lms-starter/." .
