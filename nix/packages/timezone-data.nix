{
  runCommand,
  tzdata,
  gawk,
  coreutils,
}:

runCommand "helmr-timezone-data-${tzdata.version}"
  {
    nativeBuildInputs = [
      gawk
      coreutils
    ];
  }
  ''
    set -euo pipefail
    mkdir -p "$out"
    cp -a ${tzdata}/share/zoneinfo "$out/zoneinfo"
    awk '
      ($1 == "Z" || $1 == "Zone") && NF >= 2 { print $2 }
      ($1 == "L" || $1 == "Link") && NF >= 3 { print $3 }
    ' ${tzdata}/share/zoneinfo/tzdata.zi | LC_ALL=C sort -u > "$out/tzdb_names.txt"
    test -s "$out/tzdb_names.txt"
    test "$(LC_ALL=C sort "$out/tzdb_names.txt" | uniq -d | wc -l)" -eq 0
  ''
