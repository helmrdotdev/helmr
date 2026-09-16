"""Admit the indexed Platform subset before the existing immutable publisher."""
from pathlib import Path
import sys
from contract import descriptor, read, require, safe_extract, validate
from publish import verify_signature


def materialize(index_path, signature, archive, provenance, version, source, destination):
    verify_signature(index_path, signature, version)
    index = validate(read(index_path))
    require(index['version'] == version and index['sourceCommit'] == source, 'Platform index source/tag differs')
    for name, path in (('platform-release.tar', archive), ('platform-release-provenance.json', provenance)):
        require(descriptor(path) == index['assets'][name], 'indexed Platform bytes differ: ' + name)
    proof = read(provenance)
    require(proof['formatVersion'] == 0 and proof['sourceCommit'] == source and proof['sourceRef'] == index['sourceRef'], 'Platform provenance source/ref differs')
    require(proof['archive']['digest'] == descriptor(archive)['digest'] and proof['archive']['sizeBytes'] == Path(archive).stat().st_size, 'Platform provenance archive differs')
    safe_extract(archive, destination)
    require((Path(destination) / 'platform-release.json').is_file(), 'Platform descriptor missing')


if __name__ == '__main__':
    materialize(*sys.argv[1:])
