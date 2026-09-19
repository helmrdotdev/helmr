import sharp from "sharp"

// An artifact step with a native Node library: build an image whose left half
// is red and right half blue, shrink it, rotate it a quarter turn, and read
// the pixels back.
export async function imageWork() {
  const width = 64
  const height = 48
  const pixels = Buffer.alloc(width * height * 3)
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      pixels[(y * width + x) * 3 + (x < width / 2 ? 0 : 2)] = 255
    }
  }
  const png = await sharp(pixels, { raw: { width, height, channels: 3 } }).png().toBuffer()
  // Separate pipelines: within one, sharp orders rotation ahead of resizing.
  const resized = await sharp(png).resize(32, 24, { kernel: "nearest" }).png().toBuffer()
  const transformed = await sharp(resized).rotate(90).png().toBuffer()
  const metadata = await sharp(transformed).metadata()
  const { data, info } = await sharp(transformed).raw().toBuffer({ resolveWithObject: true })
  const pixel = (x: number, y: number) => Array.from(data.subarray((y * info.width + x) * info.channels, (y * info.width + x) * info.channels + 3))
  return {
    versions: sharp.versions,
    format: metadata.format,
    width: metadata.width,
    height: metadata.height,
    top: pixel(12, 2),
    bottom: pixel(12, 29),
  }
}
