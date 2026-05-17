# Images

Guest boot images and local image fixtures live here.

Current contents:
- `guest/`: Firecracker guest boot image for running task bodies.

Image recipes should stay focused on boot artifacts and guest filesystem layout. Runtime behavior belongs in `cmd/guestd` and adapter code belongs in `runtime/`.
