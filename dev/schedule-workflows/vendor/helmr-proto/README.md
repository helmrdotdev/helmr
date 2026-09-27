# Proto

Protocol definitions and generated TypeScript bindings live here.

Current contents:
- `program.proto`, `computer.proto`: shared runtime protocol definitions.
- `buf.gen.yaml`: Go and TypeScript generation config.
- `typescript/`: generated `@helmr/proto` computer package for TypeScript consumers.

Go protobuf bindings are generated into `internal/proto/`. `proto/typescript/` is only for TypeScript bindings.
