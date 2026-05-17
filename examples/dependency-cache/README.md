# Dependency Cache

Build dependency layers from lockfiles, then run against the GitHub checkout
workspace. The `app/` directory represents a small dependency manifest copied
into the image; the task starts in the live workspace and writes a report with
relative paths at runtime. Code-only changes update the workspace report without
rebuilding dependency layers.

```bash
helmr deploy PATH/TO/dependency-cache

helmr run dependency-cache \
  --repo OWNER/REPO \
  --ref main \
  --subpath PATH/TO/dependency-cache
```
