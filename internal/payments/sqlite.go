package payments

// Pure-Go SQLite driver: no cgo, so the service cross-compiles and the CI
// image needs no build toolchain.
import _ "modernc.org/sqlite"
