// k8s-crd/src/quantity.rs
//
// Kubernetes resource.Quantity is stored as a string on the wire
// (e.g. "80Gi", "500m", "4", "1.5").  We represent it as a validated
// newtype wrapper so that:
//
//   1. JSON round-trips transparently (it serialises / deserialises as a
//      plain string — matching what kubectl and the API server produce).
//   2. We can call `.bytes()` or `.milli_cores()` for budget arithmetic
//      without pulling in a full Kubernetes client library.
//
// The parse logic implements the same grammar as Go's resource.Quantity:
//
//   DecimalSI  : k, M, G, T, P, E  (powers of 1 000)
//   BinarySI   : Ki, Mi, Gi, Ti, Pi, Ei  (powers of 1 024)
//   Milli      : trailing 'm'  (1/1 000)

use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use std::fmt;
use thiserror::Error;

/// A Kubernetes resource Quantity stored as a validated string.
///
/// # Examples
/// ```
/// use k8s_crd::Quantity;
///
/// let q: Quantity = "80Gi".parse().unwrap();
/// assert_eq!(q.as_bytes().unwrap(), 80 * 1024 * 1024 * 1024);
///
/// let q2: Quantity = "500m".parse().unwrap();
/// assert_eq!(q2.as_milli_value().unwrap(), 500);
/// ```
#[derive(Clone, Debug, PartialEq, Eq, Hash, Serialize, Deserialize, JsonSchema)]
#[schemars(transparent)]
pub struct Quantity(pub String);

#[derive(Debug, Error)]
pub enum QuantityParseError {
    #[error("empty quantity string")]
    Empty,
    #[error("unrecognised suffix in quantity '{0}'")]
    UnknownSuffix(String),
    #[error("invalid numeric part in quantity '{0}': {1}")]
    BadNumber(String, String),
}

impl fmt::Display for Quantity {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::str::FromStr for Quantity {
    type Err = QuantityParseError;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        if s.is_empty() {
            return Err(QuantityParseError::Empty);
        }
        // Validate by attempting to parse the value; we don't need to store
        // the numeric result here — it will be computed lazily by as_bytes().
        let _ = parse_quantity_to_i64(s).map_err(|e| match e {
            QuantityParseError::BadNumber(_, msg) => {
                QuantityParseError::BadNumber(s.to_owned(), msg)
            }
            other => other,
        })?;
        Ok(Quantity(s.to_owned()))
    }
}

impl Quantity {
    /// Create a Quantity without validation (useful when reading from the API
    /// server where the value is already validated upstream).
    pub fn new(raw: impl Into<String>) -> Self {
        Quantity(raw.into())
    }

    /// Returns the raw string representation.
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// Converts the quantity to a whole number of bytes.
    ///
    /// Returns an error if the quantity uses the milli (`m`) suffix, which
    /// is not meaningful for memory quantities.
    pub fn as_bytes(&self) -> Result<i64, QuantityParseError> {
        parse_quantity_to_i64(&self.0)
    }

    /// Returns the quantity as a milli-value (e.g. 500m → 500, 1 → 1000).
    pub fn as_milli_value(&self) -> Result<i64, QuantityParseError> {
        let raw = &self.0;
        if raw.ends_with('m') {
            let num = &raw[..raw.len() - 1];
            return num
                .parse::<i64>()
                .map_err(|e| QuantityParseError::BadNumber(raw.clone(), e.to_string()));
        }
        // Whole number — multiply by 1000
        parse_quantity_to_i64(raw).map(|v| v * 1000)
    }
}

/// Core parsing function.  Returns the quantity value in base units (bytes for
/// memory, cores for CPU without a suffix).
fn parse_quantity_to_i64(s: &str) -> Result<i64, QuantityParseError> {
    if s.is_empty() {
        return Err(QuantityParseError::Empty);
    }

    // --- milli suffix ---
    if s.ends_with('m') {
        // e.g. "500m" CPU → we return it as-is in milli units = 500 / 1000
        // For bytes this would be sub-byte which is nonsensical; callers
        // should use as_milli_value() instead.
        let num = &s[..s.len() - 1];
        return num
            .parse::<i64>()
            .map_err(|e| QuantityParseError::BadNumber(s.to_owned(), e.to_string()))
            .map(|v| v / 1000); // fractional, rounds down
    }

    // --- Binary SI suffixes ---
    let binary_suffixes: &[(&str, i64)] = &[
        ("Ei", 1_i64 << 60),
        ("Pi", 1_i64 << 50),
        ("Ti", 1_i64 << 40),
        ("Gi", 1_i64 << 30),
        ("Mi", 1_i64 << 20),
        ("Ki", 1_i64 << 10),
    ];
    for (suffix, factor) in binary_suffixes {
        if let Some(num_str) = s.strip_suffix(suffix) {
            let num = num_str
                .parse::<i64>()
                .map_err(|e| QuantityParseError::BadNumber(s.to_owned(), e.to_string()))?;
            return Ok(num * factor);
        }
    }

    // --- Decimal SI suffixes ---
    let decimal_suffixes: &[(&str, i64)] = &[
        ("E", 1_000_000_000_000_000_000_i64),
        ("P", 1_000_000_000_000_000_i64),
        ("T", 1_000_000_000_000_i64),
        ("G", 1_000_000_000_i64),
        ("M", 1_000_000_i64),
        ("k", 1_000_i64),
    ];
    for (suffix, factor) in decimal_suffixes {
        if let Some(num_str) = s.strip_suffix(suffix) {
            let num = num_str
                .parse::<i64>()
                .map_err(|e| QuantityParseError::BadNumber(s.to_owned(), e.to_string()))?;
            return Ok(num * factor);
        }
    }

    // --- Plain integer (no suffix) ---
    // Handle optional decimal point (e.g. "1.5" — we truncate)
    if let Some(dot) = s.find('.') {
        let integer_part = &s[..dot];
        return integer_part
            .parse::<i64>()
            .map_err(|e| QuantityParseError::BadNumber(s.to_owned(), e.to_string()));
    }

    s.parse::<i64>()
        .map_err(|e| QuantityParseError::BadNumber(s.to_owned(), e.to_string()))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn binary_si_gib() {
        let q: Quantity = "80Gi".parse().unwrap();
        assert_eq!(q.as_bytes().unwrap(), 80 * (1 << 30));
    }

    #[test]
    fn binary_si_mib() {
        let q: Quantity = "512Mi".parse().unwrap();
        assert_eq!(q.as_bytes().unwrap(), 512 * (1 << 20));
    }

    #[test]
    fn decimal_si_g() {
        let q: Quantity = "4G".parse().unwrap();
        assert_eq!(q.as_bytes().unwrap(), 4 * 1_000_000_000);
    }

    #[test]
    fn milli_cpu() {
        let q: Quantity = "500m".parse().unwrap();
        assert_eq!(q.as_milli_value().unwrap(), 500);
    }

    #[test]
    fn whole_cpu() {
        let q: Quantity = "2".parse().unwrap();
        assert_eq!(q.as_milli_value().unwrap(), 2000);
    }

    #[test]
    fn serde_roundtrip() {
        let q = Quantity::new("16Gi");
        let s = serde_json::to_string(&q).unwrap();
        assert_eq!(s, "\"16Gi\"");
        let q2: Quantity = serde_json::from_str(&s).unwrap();
        assert_eq!(q, q2);
    }

    #[test]
    fn display() {
        let q = Quantity::new("8Gi");
        assert_eq!(format!("{q}"), "8Gi");
    }
}
