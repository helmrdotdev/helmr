'use strict';

// The managed Node executable needs the runtime harness library path only
// while its ELF loader resolves the initial dependency closure. Remove the
// bootstrap variables before any platform or user JavaScript runs so native
// child processes retain the image's own library contract.
delete process.env.LD_LIBRARY_PATH;
delete process.env.NODE_OPTIONS;
process.execPath = '/opt/helmr/runtime/bin/node';
