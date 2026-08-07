const VERT = `
attribute vec2 a_pos;
void main() { gl_Position = vec4(a_pos, 0.0, 1.0); }
`;

const FRAG = `
precision mediump float;

uniform vec2 u_res;
uniform float u_pix;
uniform float u_time;
uniform float u_horizon;
uniform float u_skyMax;
uniform float u_frost;
uniform float u_sun;

const vec3 PAPER      = vec3(0.980, 0.973, 0.957);
const vec3 SKY_DOT    = vec3(0.667, 0.749, 0.875);
const vec3 CLOUD_BASE = vec3(1.000, 0.996, 0.984);
const vec3 CLOUD_DOT  = vec3(0.725, 0.765, 0.831);
const vec3 FAR_BASE   = vec3(0.906, 0.929, 0.965);
const vec3 FAR_DOT    = vec3(0.616, 0.694, 0.820);
const vec3 MID_BASE   = vec3(0.859, 0.890, 0.941);
const vec3 MID_DOT    = vec3(0.494, 0.580, 0.729);
const vec3 NEAR_BASE  = vec3(0.788, 0.831, 0.902);
const vec3 NEAR_DOT   = vec3(0.373, 0.455, 0.600);
const vec3 FROST      = vec3(0.875, 0.914, 0.973);
const vec3 SUN_BASE   = vec3(1.000, 0.941, 0.741);
const vec3 SUN_DOT    = vec3(0.862, 0.688, 0.192);

float hash(vec2 p) {
  return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453123);
}

float vnoise(vec2 p) {
  vec2 i = floor(p);
  vec2 f = fract(p);
  f = f * f * (3.0 - 2.0 * f);
  return mix(
    mix(hash(i), hash(i + vec2(1.0, 0.0)), f.x),
    mix(hash(i + vec2(0.0, 1.0)), hash(i + vec2(1.0, 1.0)), f.x),
    f.y
  );
}

float fbm(vec2 p) {
  float v = 0.0;
  float a = 0.5;
  for (int i = 0; i < 4; i++) {
    v += a * vnoise(p);
    p *= 2.03;
    a *= 0.5;
  }
  return v;
}

float bayer2(vec2 p) {
  return p.x * 2.0 + p.y * 3.0 - 4.0 * p.x * p.y;
}

float bayer4(vec2 p) {
  vec2 p1 = mod(p, 2.0);
  vec2 p2 = mod(floor(p * 0.5), 2.0);
  return (4.0 * bayer2(p1) + bayer2(p2)) / 16.0 + 1.0 / 32.0;
}

vec3 duotone(vec3 base, vec3 dot, float density, float threshold) {
  return mix(base, dot, step(threshold, clamp(density, 0.0, 1.0)));
}

void main() {
  vec2 cell = floor(gl_FragCoord.xy / u_pix);
  vec2 p = (cell + 0.5) * u_pix;
  vec2 uv = p / u_res;
  float sx = uv.x * (u_res.x / u_res.y);

  float threshold = mix(bayer4(cell), hash(cell + 0.37), 0.35);

  float H = u_horizon;
  float hFar  = H * 0.86 + H * 0.015 * (fbm(vec2(sx * 1.6 + 11.3, 2.0)) - 0.5);
  float hMid  = H * 0.56 + H * 0.07 * sin(sx * 3.4 + 4.1)
              + H * 0.05 * (fbm(vec2(sx * 2.6 + 43.0, 7.0)) - 0.5);
  float hNear = H * 0.28 + H * 0.06 * sin(sx * 5.2 + 8.6)
              + H * 0.05 * (fbm(vec2(sx * 3.4 + 87.0, 4.0)) - 0.5);

  vec3 color;

  if (uv.y < hNear) {
    float d = 0.42 + 0.45 * smoothstep(0.0, H * 0.7, hNear - uv.y);
    d += 0.20 * (fbm(vec2(sx * 1.3, uv.y * 46.0)) - 0.5);
    d -= 0.38 * (1.0 - smoothstep(0.0, H * 0.07, hNear - uv.y));
    color = duotone(NEAR_BASE, NEAR_DOT, d, threshold);
  } else if (uv.y < hMid) {
    float d = 0.36 + 0.42 * smoothstep(0.0, H * 0.7, hMid - uv.y);
    d += 0.18 * (fbm(vec2(sx * 1.5 + 40.0, uv.y * 40.0)) - 0.5);
    d -= 0.34 * (1.0 - smoothstep(0.0, H * 0.06, hMid - uv.y));
    color = duotone(MID_BASE, MID_DOT, d, threshold);
  } else if (uv.y < hFar) {
    float d = 0.26 + 0.34 * smoothstep(0.0, H * 0.8, hFar - uv.y);
    d += 0.16 * (fbm(vec2(sx * 1.8 + 80.0, uv.y * 34.0)) - 0.5);
    d -= 0.30 * (1.0 - smoothstep(0.0, H * 0.05, hFar - uv.y));
    color = duotone(FAR_BASE, FAR_DOT, d, threshold);
  } else {
    float lift = smoothstep(H + 0.10, 1.05, uv.y);
    float sky = u_skyMax * pow(lift, 1.7);
    sky += 0.10 * sky * (fbm(vec2(sx * 1.3, uv.y * 2.4)) - 0.5);
    sky += 0.10 * (1.0 - smoothstep(H, H + 0.10, uv.y));

    float cloudGate = step(0.001, u_skyMax);
    vec2 drift = vec2(u_time * 0.014, 0.0);
    float cshape = fbm(vec2(sx * 0.95, uv.y * 3.0) + drift);
    cshape += 0.35 * fbm(vec2(sx * 2.6, uv.y * 7.0) + drift * 2.2);
    cshape /= 1.35;
    cshape *= 0.74 + 0.40 * smoothstep(0.35, 0.85, uv.y);
    float cloudSoft = smoothstep(0.55, 0.61, cshape) * cloudGate;
    float cloudMask = step(threshold, cloudSoft);

    float fringe = smoothstep(0.60, 0.565, cshape)
      * smoothstep(0.515, 0.565, cshape);
    float shadow = fringe * 0.5 * cloudGate;

    float skyDensity = mix(sky, 0.02, cloudSoft);
    vec3 skyColor = duotone(PAPER, SKY_DOT, skyDensity, threshold);
    vec3 shadowColor = duotone(CLOUD_BASE, CLOUD_DOT, shadow * lift, threshold);
    color = mix(skyColor, shadowColor, step(0.001, shadow) * (1.0 - cloudMask));
    color = mix(color, duotone(CLOUD_BASE, SKY_DOT, 0.05, threshold), cloudMask);

    float aspect = u_res.x / u_res.y;
    float sr = H * 0.30;
    vec2 sunAt = vec2(max(0.075 * aspect, sr * 1.2), H * 1.34);
    float sd = distance(vec2(sx, uv.y), sunAt);
    float sunMask = u_sun * step(threshold, 1.0 - smoothstep(sr * 0.82, sr * 1.02, sd));
    float sunD = 0.30 + 0.35 * smoothstep(sr, sr * 0.2, sd);
    color = mix(color, duotone(SUN_BASE, SUN_DOT, sunD, threshold), sunMask);
  }

  color = mix(color, FROST, u_frost * 0.22);
  gl_FragColor = vec4(color, 1.0);
}
`;

export type DitherScene = {
  freeze(): void;
  resume(): void;
  destroy(): void;
};

export type DitherSceneOptions = {
  /** Height of the far-hill horizon, in CSS px measured from the bottom. */
  horizonPx?: number;
  /** CSS px per dither cell. */
  cell?: number;
  /** Peak sky stipple density, 0..1. */
  skyMax?: number;
  /** When false, render a single static frame (reduced motion). */
  animate?: boolean;
  /** Draw the dithered sun over the far ridge. */
  sun?: boolean;
  /** Initial frost amount, 0..1. */
  frost?: number;
};

export function mountDitherScene(
  canvas: HTMLCanvasElement,
  options: DitherSceneOptions = {},
): DitherScene | null {
  const {
    horizonPx = 190,
    cell = 2,
    skyMax = 0.42,
    animate = true,
    sun = true,
    frost: frostInit = 0,
  } = options;

  const gl = canvas.getContext("webgl", {
    antialias: false,
    depth: false,
    stencil: false,
    alpha: false,
    powerPreference: "low-power",
  });
  if (!gl) return null;

  const compile = (type: number, source: string) => {
    const shader = gl.createShader(type);
    if (!shader) return null;
    gl.shaderSource(shader, source);
    gl.compileShader(shader);
    if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
      gl.deleteShader(shader);
      return null;
    }
    return shader;
  };

  const vert = compile(gl.VERTEX_SHADER, VERT);
  const frag = compile(gl.FRAGMENT_SHADER, FRAG);
  const program = gl.createProgram();
  if (!vert || !frag || !program) return null;
  gl.attachShader(program, vert);
  gl.attachShader(program, frag);
  gl.linkProgram(program);
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) return null;
  gl.useProgram(program);

  const quad = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, quad);
  gl.bufferData(
    gl.ARRAY_BUFFER,
    new Float32Array([-1, -1, 3, -1, -1, 3]),
    gl.STATIC_DRAW,
  );
  const aPos = gl.getAttribLocation(program, "a_pos");
  gl.enableVertexAttribArray(aPos);
  gl.vertexAttribPointer(aPos, 2, gl.FLOAT, false, 0, 0);

  const uRes = gl.getUniformLocation(program, "u_res");
  const uPix = gl.getUniformLocation(program, "u_pix");
  const uTime = gl.getUniformLocation(program, "u_time");
  const uHorizon = gl.getUniformLocation(program, "u_horizon");
  const uSkyMax = gl.getUniformLocation(program, "u_skyMax");
  const uFrost = gl.getUniformLocation(program, "u_frost");
  const uSun = gl.getUniformLocation(program, "u_sun");

  let width = 0;
  let height = 0;
  let dpr = 1;

  const resize = () => {
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    const w = Math.max(1, Math.round(canvas.clientWidth * dpr));
    const h = Math.max(1, Math.round(canvas.clientHeight * dpr));
    if (w === width && h === height) return false;
    width = w;
    height = h;
    canvas.width = w;
    canvas.height = h;
    gl.viewport(0, 0, w, h);
    return true;
  };

  let sceneTime = 0;
  let frost = frostInit;
  let frostTarget = frostInit;
  let frozen = false;
  let raf = 0;
  let last = 0;
  let destroyed = false;

  const draw = () => {
    gl.uniform2f(uRes, width, height);
    gl.uniform1f(uPix, cell * dpr);
    gl.uniform1f(uTime, sceneTime);
    gl.uniform1f(uHorizon, Math.min(0.9, (horizonPx * dpr) / height));
    gl.uniform1f(uSkyMax, skyMax);
    gl.uniform1f(uFrost, frost);
    gl.uniform1f(uSun, sun ? 1 : 0);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
  };

  const settled = () => frost === frostTarget && (frozen || !animate);

  const tick = (now: number) => {
    raf = 0;
    if (destroyed) return;
    const dt = Math.min(0.1, (now - last) / 1000 || 0);
    last = now;
    if (!frozen) sceneTime += dt;
    if (frost !== frostTarget) {
      const step = dt / 0.6;
      frost = frostTarget > frost
        ? Math.min(frostTarget, frost + step)
        : Math.max(frostTarget, frost - step);
    }
    resize();
    draw();
    if (!settled()) raf = requestAnimationFrame(tick);
  };

  const wake = () => {
    if (destroyed || raf) return;
    last = performance.now();
    raf = requestAnimationFrame(tick);
  };

  const observer = new ResizeObserver(() => {
    if (destroyed) return;
    if (resize()) {
      draw();
      wake();
    }
  });
  observer.observe(canvas);

  resize();
  draw();
  if (animate) wake();

  return {
    freeze() {
      frozen = true;
      frostTarget = 1;
      wake();
    },
    resume() {
      frozen = false;
      frostTarget = 0;
      if (animate) wake();
    },
    destroy() {
      destroyed = true;
      if (raf) cancelAnimationFrame(raf);
      observer.disconnect();
      gl.getExtension("WEBGL_lose_context")?.loseContext();
    },
  };
}
