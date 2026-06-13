package account

// MaxGracePeriodSec caps gracePeriodSec / invalidateGracePeriodSec to prevent
// int64 overflow when computing now + grace*time.Second. Set to 366 days
// (one leap year) — beyond which an operator should rotate keys instead of
// holding a long grace window. Exceeding this limit returns 400 BAD_REQUEST.
const MaxGracePeriodSec = int64(366 * 24 * 3600) // 31622400 seconds = 366 days
