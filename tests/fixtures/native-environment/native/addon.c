#define _GNU_SOURCE
#include <node_api.h>
#include <aio.h>
#include <fcntl.h>
#include <resolv.h>
#include <sqlite3.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

// Exercises libsqlite3 from the distribution plus glibc components that Node
// itself does not link: librt (shm, aio) and libresolv.
static napi_value Run(napi_env env, napi_callback_info info) {
  struct __res_state state;
  memset(&state, 0, sizeof state);
  int resolver = res_ninit(&state);
  res_nclose(&state);

  int shm = shm_open("/helmr-native-environment", O_CREAT | O_RDWR, 0600);
  if (shm >= 0) {
    close(shm);
    shm_unlink("/helmr-native-environment");
  }

  struct aiocb request;
  memset(&request, 0, sizeof request);
  char buffer[8];
  request.aio_fildes = open("/proc/self/cmdline", O_RDONLY);
  request.aio_buf = buffer;
  request.aio_nbytes = sizeof buffer;
  int aio = aio_read(&request);
  if (aio == 0) {
    const struct aiocb *pending[1] = {&request};
    aio_suspend(pending, 1, NULL);
    aio = aio_error(&request);
  }
  if (request.aio_fildes >= 0) close(request.aio_fildes);

  char out[256];
  snprintf(out, sizeof out, "{\"sqlite\":\"%s\",\"resolver\":%d,\"shm\":%s,\"aio\":%d}",
           sqlite3_libversion(), resolver, shm >= 0 ? "true" : "false", aio);
  napi_value result;
  napi_create_string_utf8(env, out, NAPI_AUTO_LENGTH, &result);
  return result;
}

static napi_value Init(napi_env env, napi_value exports) {
  napi_value fn;
  napi_create_function(env, "run", NAPI_AUTO_LENGTH, Run, NULL, &fn);
  napi_set_named_property(env, exports, "run", fn);
  return exports;
}

NAPI_MODULE(NODE_GYP_MODULE_NAME, Init)
