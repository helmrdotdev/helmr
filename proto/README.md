# Proto

Protocol definitions and generated TypeScript bindings live here.

Current contents:
- `bundle.proto`, `run.proto`: shared protocol definitions.
- `buf.gen.yaml`: Go and TypeScript generation config.
- `typescript/`: generated `@helmr/proto` workspace package for TypeScript consumers.

Go protobuf bindings are generated into `internal/gen/`. `proto/typescript/` is only for TypeScript bindings.
