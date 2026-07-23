# Images

Guest boot images and local image fixtures live here.

Current contents:
- `guest/`: Firecracker guest boot image for running task bodies.

Image recipes should stay focused on boot artifacts and guest filesystem layout. Runtime behavior belongs in `internal/guestd` and Program code belongs in `runtime/`.
