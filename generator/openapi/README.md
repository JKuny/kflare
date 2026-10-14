# The OpenAPI subset kflare-gen reads

`openapi.json` holds the operations and components of Cloudflare's OpenAPI spec that the kinds in
`../generator.yaml` and `../testdata/generator.yaml` use. It comes from
[cloudflare/api-schemas](https://github.com/cloudflare/api-schemas) at the commit its `info.x-kflare-source` names,
under that repository's BSD 3-Clause license (`LICENSE`).

Do not edit it by hand. `make sync-spec` rewrites both files from the commit `API_SCHEMAS_COMMIT` (in the Makefile)
names: the last spec before the release of the cloudflare-go v7 that `go.mod` requires. Run it after adding a kind or
moving the SDK, and review what changed: the subset's diff is where a constraint Cloudflare changed shows up.
