## Summary

-

## Verification

- [ ] `CGO_ENABLED=1 go vet ./...`
- [ ] `CGO_ENABLED=1 go build ./...`
- [ ] `CGO_ENABLED=1 go test -race ./...`

## Checklist

- [ ] stdout/stderr contract is preserved
- [ ] Tests cover changed behavior
- [ ] README or changelog updated when user-facing behavior changed
- [ ] No out-of-scope protocol/auth/compression/control-channel work included
