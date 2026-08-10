package guest

// Darwin exposes /tmp as a system-owned symlink to /private/tmp. The guest
// helper runs on Linux, but host-side unit tests use the canonical root.
const guestTemporaryRootPath = "/private/tmp"
