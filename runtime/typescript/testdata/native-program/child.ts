const value: number = 42
process.send!(value, () => process.disconnect())
