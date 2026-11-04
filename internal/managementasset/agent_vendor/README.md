# Agent Workspace browser dependencies

These upstream browser distributions are embedded into the CPA binary and served
locally. No browser CDN requests are required.

| Package | Version | Upstream file | License |
| --- | --- | --- | --- |
| marked | 18.1.0 | lib/marked.umd.js | MIT |
| dompurify | 3.4.16 | dist/purify.min.js | Apache-2.0 OR MPL-2.0 |
| lucide | 1.53.0 | dist/umd/lucide.min.js | ISC |

Obtain the pinned packages from the npm registry with lifecycle scripts disabled.
Copy the browser distributions and the corresponding license files into this
directory. The workspace sanitizes all Markdown HTML with DOMPurify; model output
must never be inserted as unsanitized HTML.
