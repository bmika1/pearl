//! Checks committed-LUT lookups and accumulates their per-proof multiplicities.

use std::collections::BTreeMap;

use plonky2::field::polynomial::PolynomialValues;
use plonky2::field::types::{Field, PrimeField64};

use super::LutTable;
use super::columns::{lut_height, num_slots, slot_height};
use super::ctl::LutLookup;
use super::stark::generate;

/// The per-proof half of the committed oracle: for every table, one count column per slot —
/// `counts[device LUT position][slot][row]`. Filled by
/// [`LutChecker::check_trace`] (or [`Self::add`]) and turned into trace columns by
/// [`Self::table_columns`] / [`super::stark::lut_trace`]. Honest counts are far below the field order
/// (instances x trace height), so the field encoding is exact.
#[derive(Clone, Debug)]
pub struct LutMultiplicities {
    tables: Vec<LutTable>,
    counts: Vec<Vec<Vec<u64>>>,
}

impl LutMultiplicities {
    pub fn new(tables: &[LutTable]) -> Self {
        let counts = tables
            .iter()
            .map(|&t| vec![vec![0u64; lut_height(t)]; num_slots(t)])
            .collect();
        Self {
            tables: tables.to_vec(),
            counts,
        }
    }

    /// The device-family position of `table`.
    fn table_position(&self, table: LutTable) -> usize {
        self.tables
            .iter()
            .position(|&t| t == table)
            .unwrap_or_else(|| panic!("{table:?} is not committed for this device"))
    }

    /// Resolves one looked key tuple to its `(slot, row)`. `Err` when the tuple falls outside
    /// the table's committed domain — for an honest trace that is a bug in the trace or the
    /// descriptor (several tables use the key domain itself as a range proof). XOR8 selects
    /// its slot by the tag it binds as its last value.
    pub fn resolve(table: LutTable, keys: &[u64], values: &[u64]) -> Result<(usize, usize), String> {
        let fold = |width: u32| -> Result<(usize, usize), String> {
            let (slot, row) = ((keys[0] >> width) as usize, (keys[0] & ((1 << width) - 1)) as usize);
            if slot >= num_slots(table) {
                return Err(format!("{table:?} key {} addresses nonexistent slot {slot}", keys[0]));
            }
            Ok((slot, row))
        };
        let single_slot = |row: u64| -> Result<(usize, usize), String> {
            if (row as usize) < slot_height(table) {
                Ok((0, row as usize))
            } else {
                Err(format!("{table:?} key {row} out of domain [0, {})", slot_height(table)))
            }
        };
        let expected_keys = match table {
            LutTable::Bytes2 | LutTable::Pair128 | LutTable::Xor8 => 2,
            _ => 1,
        };
        if keys.len() != expected_keys {
            return Err(format!(
                "{table:?} lookup carries {} key components, wants {expected_keys}",
                keys.len()
            ));
        }
        match table {
            // The tuple components must be in range individually — a component overflow must
            // not alias into another row's valid tuple.
            LutTable::Bytes2 => {
                if keys[0] >= 256 || keys[1] >= 256 {
                    return Err(format!("BYTES2 tuple ({}, {}) has an oversized byte", keys[0], keys[1]));
                }
                Ok((0, (keys[0] + (keys[1] << 8)) as usize))
            }
            LutTable::Pair128 => {
                if keys[0] >= 128 || keys[1] >= 128 {
                    return Err(format!("PAIR128 tuple ({}, {}) has an oversized half", keys[0], keys[1]));
                }
                Ok((0, (keys[0] + (keys[1] << 7)) as usize))
            }
            LutTable::Xor8 => {
                if keys[0] >= 256 || keys[1] >= 256 {
                    return Err(format!("XOR8 tuple ({}, {}) has an oversized byte", keys[0], keys[1]));
                }
                let slot = match values {
                    [_, _, tag] if (*tag as usize) < num_slots(table) => *tag as usize,
                    _ => return Err(format!("XOR8 values {values:?} carry no valid slot tag")),
                };
                Ok((slot, (keys[0] + (keys[1] << 8)) as usize))
            }
            // WIDTH32's stored key is the shifted ramp [1, 32]: row = key - 1. Key 0 has no
            // row — this is the "no zero-width claim" soundness point.
            LutTable::Width32 => {
                if (1..=slot_height(table) as u64).contains(&keys[0]) {
                    Ok((0, keys[0] as usize - 1))
                } else {
                    Err(format!("WIDTH32 key {} out of domain [1, 32]", keys[0]))
                }
            }
            LutTable::RneRnd => fold(17),
            LutTable::ProdAlign15 | LutTable::B200Align | LutTable::WidthNorm => fold(16),
            LutTable::Pow2G => match keys[0] {
                0..=63 => Ok((0, keys[0] as usize)),
                128..=191 => Ok((1, keys[0] as usize - 128)),
                192..=217 => Ok((2, keys[0] as usize - 192)),
                257..=305 => Ok((3, keys[0] as usize - 257)),
                key => Err(format!("POW2G key {key} outside carry, gap, and normalization domains")),
            },
            _ => single_slot(keys[0]),
        }
    }

    /// Records `mult` lookups of `keys` (bound to `values`) into `table`.
    pub fn add(&mut self, table: LutTable, keys: &[u64], values: &[u64], mult: u64) -> Result<(), String> {
        let (slot, row) = Self::resolve(table, keys, values)?;
        let table_position = self.table_position(table);
        self.counts[table_position][slot][row] += mult;
        Ok(())
    }

    /// One table's multiplicity columns, in slot order — the AIR's trailing columns.
    pub fn table_columns<F: Field>(&self, table: LutTable) -> Vec<Vec<F>> {
        self.counts[self.table_position(table)]
            .iter()
            .map(|col| col.iter().map(|&c| F::from_canonical_u64(c)).collect())
            .collect()
    }

    /// Total lookups recorded into `table` (all slots, all rows).
    pub fn table_total(&self, table: LutTable) -> u64 {
        self.counts[self.table_position(table)].iter().flatten().sum()
    }
}

/// Debug/test-side oracle checker: walks LUT instance inventories over honest traces, checks
/// every instance is *served* by the committed tables (key resolves in-domain, bound values
/// equal the generated outputs), and accumulates the per-slot multiplicities — the committed-LUT
/// analogue of `starky::cross_table_lookup::debug_utils::check_ctls`, with per-instance error
/// reporting the multiset check cannot give.
pub struct LutChecker<F: PrimeField64> {
    pub multiplicities: LutMultiplicities,
    _phantom: core::marker::PhantomData<F>,
}

impl<F: PrimeField64> LutChecker<F> {
    pub fn new(tables: &[LutTable]) -> Self {
        Self {
            multiplicities: LutMultiplicities::new(tables),
            _phantom: core::marker::PhantomData,
        }
    }

    /// Checks every instance of `lookups` on every row of `trace` (column-major poly values),
    /// reading filter values as multiplicities (the logup numerator semantics; honest filters
    /// are 0/1). `ctx` tags error messages. `public_inputs` is the host table's public-input
    /// vector (Scale's T3 gate keys read the `DEAD_LIMIT` slots).
    ///
    /// Rows are processed in parallel chunks, each accumulating into its own multiplicity
    /// counters; the chunks merge exactly (u64 addition commutes), and the first error in row
    /// order — identical to the serial walk — is reported. Per-chunk state stays minimal:
    /// counters are allocated only for the tables this inventory touches, and the generated
    /// table outputs are materialized once and shared immutably across chunks.
    pub fn check_trace(
        &mut self,
        lookups: &[LutLookup<F>],
        trace: &[PolynomialValues<F>],
        public_inputs: &[F],
        ctx: &str,
    ) -> Result<(), String> {
        use plonky2_maybe_rayon::*;

        let num_rows = trace[0].len();
        if num_rows == 0 {
            return Ok(());
        }
        // Chunk boundaries: fixed-size chunks of >= 2^16 rows, capped at 256
        // chunks — enough to fill any pool while keeping per-chunk counter
        // allocation amortized. The big tables (2^21..2^22 rows) dominate;
        // smaller ones fall back to one chunk.
        let chunk_rows = (num_rows / 256).max(1 << 16).min(num_rows);
        let num_chunks = num_rows.div_ceil(chunk_rows);
        debug_assert!(chunk_rows * num_chunks >= num_rows);

        // The distinct tables this inventory touches — chunk counters are
        // allocated for these only, not for the full committed set.
        let inventory_tables: Vec<LutTable> = {
            let mut t: Vec<LutTable> = lookups.iter().map(|l| l.table).collect();
            t.sort_unstable();
            t.dedup();
            t
        };
        // Generated outputs, materialized once and shared across chunks
        // (the generator is deterministic per (table, slot)).
        let outputs: BTreeMap<(LutTable, usize), Vec<Vec<F>>> = {
            let mut m = BTreeMap::new();
            for &table in &inventory_tables {
                for slot in 0..num_slots(table) {
                    m.insert((table, slot), generate::<F>(table, slot));
                }
            }
            m
        };

        let chunk_results: Vec<Result<LutMultiplicities, String>> = (0..num_chunks)
            .into_par_iter()
            .map(|chunk| {
                let row_lo = chunk * chunk_rows;
                let row_hi = (row_lo + chunk_rows).min(num_rows);
                // Counters for this chunk: only the inventory's tables.
                let mut counts = LutMultiplicities::new(&inventory_tables);
                for (li, lookup) in lookups.iter().enumerate() {
                    let table_position = counts.table_position(lookup.table);
                    let chunk_counts = &mut counts.counts[table_position];
                    for row in row_lo..row_hi {
                        let err = |e: String| format!("{ctx} lookup {li} ({:?}) row {row}: {e}", lookup.table);
                        let mult = lookup.filter.eval_table(trace, row, public_inputs).to_canonical_u64();
                        if mult == 0 {
                            continue;
                        }
                        if mult > 1 << 20 {
                            return Err(err(format!("implausible filter/multiplicity value {mult}")));
                        }
                        let keys: Vec<u64> = lookup
                            .keys
                            .iter()
                            .map(|c| c.eval_table(trace, row, public_inputs).to_canonical_u64())
                            .collect();
                        let values: Vec<u64> = lookup
                            .values
                            .iter()
                            .map(|c| c.eval_table(trace, row, public_inputs).to_canonical_u64())
                            .collect();
                        let (slot, table_row) = LutMultiplicities::resolve(lookup.table, &keys, &values).map_err(err)?;
                        let outputs = &outputs[&(lookup.table, slot)];
                        // BYTES2/PAIR128 bind only their key tuple, already checked by resolution.
                        let expected_arity = match lookup.table {
                            LutTable::Bytes2 | LutTable::Pair128 => 0,
                            _ => outputs.len(),
                        };
                        if values.len() != expected_arity {
                            return Err(err(format!("binds {} values, table returns {expected_arity}", values.len())));
                        }
                        for (i, &got) in values.iter().enumerate() {
                            let want = outputs[i][table_row].to_canonical_u64();
                            if got != want {
                                return Err(err(format!(
                                    "value {i} = {got:?} differs from {want:?} (slot {slot}, table row {table_row})"
                                )));
                            }
                        }
                        chunk_counts[slot][table_row] += mult;
                    }
                }
                Ok(counts)
            })
            .collect();

        // Merge in row order; surface the first error exactly as the serial walk would.
        let mut first_err = None;
        for res in chunk_results {
            match res {
                Ok(counts) => {
                    for (table_position, src) in counts.counts.into_iter().enumerate() {
                        let table = counts.tables[table_position];
                        let dst_position = self.multiplicities.table_position(table);
                        for (dst_slot, src_slot) in
                            self.multiplicities.counts[dst_position].iter_mut().zip(src)
                        {
                            for (dst_row, src_row) in dst_slot.iter_mut().zip(src_slot) {
                                *dst_row += src_row;
                            }
                        }
                    }
                }
                Err(e) => {
                    if first_err.is_none() {
                        first_err = Some(e);
                    }
                }
            }
        }
        if let Some(e) = first_err {
            return Err(e);
        }
        Ok(())
    }
}
