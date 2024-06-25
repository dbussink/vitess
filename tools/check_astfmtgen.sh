#!/bin/bash

#
# Validate that the current version of ast_format_fast matches what gets generated.
#

source build.env

first_output=$(git status --porcelain)

go run ./go/tools/astfmtgen vitess.io/vitess/go/vt/sqlparser/...

second_output=$(git status --porcelain)

diff=$(diff <( echo "$first_output") <( echo "$second_output"))

if [[ "$diff" != "" ]]; then
  echo "ERROR: Regenerated cached_size files do not match the current version."
  echo -e "List of files containing differences:\n$diff"
  exit 1
fi
