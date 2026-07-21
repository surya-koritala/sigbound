<!-- Thanks for contributing to Sigbound. Keep changes focused. -->

## Summary

What does this change and why?

## Related issues

Closes #

## Checklist

- [ ] `go build ./...` and `go vet ./...` pass
- [ ] `go test -race ./...` passes
- [ ] `gofmt -l .` is clean (no output)
- [ ] Correctness is preserved — `-verify` gating is not weakened, lanes stay
      disjoint, and the merge/integration path still refuses unverified code
- [ ] New parsing of git or model output has a fuzz target + seed corpus
- [ ] New behavior is covered by tests (and a `sigbench` scenario if it touches
      the merge path)
- [ ] Docs / README updated if user-facing behavior changed
