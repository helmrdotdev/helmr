# Dependency Cache

Build dependency layers from lockfiles, then run with an empty writable
workspace. The `app/` directory represents a small dependency manifest copied
into the image; the session starts in the live workspace and writes a report with
relative paths at runtime. Code-only changes update the workspace report without
rebuilding dependency layers.

```bash
helmr deploy PATH/TO/dependency-cache

helmr session start dependency-cache
```
