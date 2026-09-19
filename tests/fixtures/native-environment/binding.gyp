{
  "targets": [
    {
      "target_name": "distro_addon",
      "sources": ["native/addon.c"],
      "libraries": ["-Wl,--no-as-needed", "-lrt", "-lresolv", "-lsqlite3"]
    }
  ]
}
