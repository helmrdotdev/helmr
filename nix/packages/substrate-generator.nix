{ e2fsprogs }:
e2fsprogs.overrideAttrs (old: {
  patches = (old.patches or [ ]) ++ [ ./e2fsprogs-archive-xattrs.patch ];
})
