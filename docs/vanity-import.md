# Go import path

This module's path is `setdrift.io/setdrift-engine`, a vanity path for
https://github.com/setdrift/setdrift-engine: the code can move between
hosts without breaking anyone's imports.

## Serving it

`go get setdrift.io/setdrift-engine/...` fetches
`https://setdrift.io/setdrift-engine?go-get=1`, and the same for every
package under it (e.g. `https://setdrift.io/setdrift-engine/pkg/collect?go-get=1`).
Each must return an HTML page with these tags in its `<head>`:

```html
<meta name="go-import" content="setdrift.io/setdrift-engine git https://github.com/setdrift/setdrift-engine">
<meta name="go-source" content="setdrift.io/setdrift-engine https://github.com/setdrift/setdrift-engine https://github.com/setdrift/setdrift-engine/tree/main{/dir} https://github.com/setdrift/setdrift-engine/blob/main{/dir}/{file}#L{line}">
```

Serve the same page for `/setdrift-engine` and every path under it; any
static host with a rewrite rule works (for example a CloudFront function, or
S3 website routing rules). `go-source` lets pkg.go.dev link to the source.
