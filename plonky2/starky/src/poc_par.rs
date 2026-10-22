//! Throwaway switches for parallelism probes. Off unless the env var is set.
//! `POC_PAR_INV=32` splits long Montgomery inverses into that many independent
//! chunks. `POC_PAR_COLS=1` parallelizes helper-column chunks inside one lookup.
//! `POC_PAR_EVAL=1` parallelizes column evaluation across row chunks.

use plonky2_maybe_rayon::*;

#[cfg(feature = "std")]
fn env_usize(name: &str) -> usize {
    std::env::var(name)
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(0)
}

#[cfg(not(feature = "std"))]
fn env_usize(_name: &str) -> usize {
    0
}

pub(crate) fn par_inv_chunks() -> usize {
    env_usize("POC_PAR_INV")
}

pub(crate) fn par_cols() -> bool {
    env_usize("POC_PAR_COLS") == 1
}

pub(crate) fn par_eval() -> bool {
    env_usize("POC_PAR_EVAL") == 1
}

pub(crate) fn stamp(label: &str) {
    #[cfg(feature = "std")]
    if env_usize("POC_STAMP") == 1 {
        let ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis())
            .unwrap_or(0);
        eprintln!("POCSTAMP {label} {ms}");
    }
}

/// Montgomery inverse, optionally split into independent chunks (bit-exact:
/// each chunk inverts only its own elements).
pub(crate) fn batch_inverse<F: plonky2::field::types::Field>(x: &[F]) -> Vec<F> {
    let chunks = par_inv_chunks();
    if chunks <= 1 || x.len() < 4096 {
        return F::batch_multiplicative_inverse(x);
    }
    let n = x.len();
    let chunk_len = n.div_ceil(chunks);
    x.par_chunks(chunk_len)
        .flat_map(|c| F::batch_multiplicative_inverse(c))
        .collect()
}
