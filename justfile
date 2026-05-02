default:
  @just --list

[private]
run-all recipe:
  just -f go/justfile -d go {{recipe}}
  just -f py/justfile -d py {{recipe}}

build:
  @just run-all build
  mkdir -p bin
  ln -sf ../go/bin/ptrack bin/ptrack
  ln -sf ../py/bin/ptrack_py bin/ptrack_py

test:
  @just run-all test

lint:
  @just run-all lint

fmt:
  @just run-all fmt
