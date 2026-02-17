## Summary

## Validation

- [ ] `go list ./... | grep -v '/tests/e2e' | xargs go test -count=1`
- [ ] `helm template spotvortex charts/spotvortex --set apiKey=dummy`
- [ ] `docker build -t spotvortex-agent:local .`
- [ ] e2e (if relevant)

## Notes
