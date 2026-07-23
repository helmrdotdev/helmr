# Dependency Cache

Build dependency layers from lockfiles, then run with an empty writable
workspace. The `app/` directory represents a small dependency manifest copied
into the image. The Task writes a report with relative paths in the live
Workspace. Code-only changes update the Workspace report without
rebuilding dependency layers.

```bash
helmr deploy PATH/TO/dependency-cache
```
