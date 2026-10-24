use std::collections::VecDeque;

/// Bounded byte queue; drops oldest data when over capacity.
pub struct ByteRing {
    deque: VecDeque<u8>,
    capacity: usize,
}

impl ByteRing {
    pub fn with_capacity(capacity: usize) -> Self {
        Self {
            deque: VecDeque::with_capacity(capacity.min(4096)),
            capacity,
        }
    }

    pub fn push(&mut self, data: &[u8]) {
        for &b in data {
            if self.deque.len() >= self.capacity {
                self.deque.pop_front();
            }
            self.deque.push_back(b);
        }
    }

    pub fn pop_into(&mut self, out: &mut [u8]) -> usize {
        let n = out.len().min(self.deque.len());
        for i in 0..n {
            out[i] = self.deque.pop_front().unwrap_or(0);
        }
        n
    }
}
