#include <node_api.h>

// Linked against a stub libc that claims a symbol version no real glibc has,
// standing in for a library built against a newer glibc than the Runtime's.
extern int helmr_future(void);

static napi_value Init(napi_env env, napi_value exports) {
  napi_value value;
  napi_create_int32(env, helmr_future(), &value);
  napi_set_named_property(env, exports, "value", value);
  return exports;
}

NAPI_MODULE(NODE_GYP_MODULE_NAME, Init)
