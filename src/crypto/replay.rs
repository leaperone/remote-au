/// Anti-replay bitmap. Call only after authentication succeeds.
/// Sequence numbers that slide out of the window stay rejected.
#[derive(Debug, Clone)]
pub struct ReplayWindow {
    window: u64,
    base: u64,
    bits: Vec<u64>,
}

impl ReplayWindow {
    pub fn new(window: usize) -> Self {
        let window = window.clamp(64, 4096);
        let words = window.div_ceil(64);
        Self {
            window: window as u64,
            base: 0,
            bits: vec![0; words],
        }
    }

    pub fn commit(&mut self, seq: u64) -> bool {
        if seq == 0 {
            return false;
        }
        if self.base == 0 {
            self.base = seq.saturating_sub(self.window - 1).max(1);
            self.clear();
            return self.mark(seq);
        }
        if seq < self.base {
            return false;
        }
        if seq - self.base >= self.window {
            let new_base = seq - self.window + 1;
            self.slide(new_base);
        }
        self.mark(seq)
    }

    fn mark(&mut self, seq: u64) -> bool {
        let idx = (seq - self.base) as usize;
        if idx >= self.window as usize {
            return false;
        }
        let word = idx / 64;
        let mask = 1u64 << (idx % 64);
        if self.bits[word] & mask != 0 {
            return false;
        }
        self.bits[word] |= mask;
        true
    }

    fn clear(&mut self) {
        for word in &mut self.bits {
            *word = 0;
        }
    }

    fn slide(&mut self, new_base: u64) {
        if new_base <= self.base {
            return;
        }
        let shift = new_base - self.base;
        if shift >= self.window {
            self.clear();
            self.base = new_base;
            return;
        }
        let shift = shift as usize;
        let width = self.window as usize;
        let mut next = vec![0u64; self.bits.len()];
        for old in shift..width {
            let word = old / 64;
            let mask = 1u64 << (old % 64);
            if self.bits[word] & mask == 0 {
                continue;
            }
            let idx = old - shift;
            next[idx / 64] |= 1u64 << (idx % 64);
        }
        self.bits = next;
        self.base = new_base;
    }
}
