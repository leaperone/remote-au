/// A process timer request for 5 ms sender pacing. No persistent OS setting.
#[cfg(windows)]
pub struct ClockGuard {
    active: bool,
}
#[cfg(not(windows))]
pub struct ClockGuard;

#[cfg(windows)]
#[link(name = "winmm")]
extern "system" {
    fn timeBeginPeriod(period: u32) -> u32;
    fn timeEndPeriod(period: u32) -> u32;
}

pub fn request_low_latency_clock() -> ClockGuard {
    #[cfg(windows)]
    {
        // Both WinMM functions accept a millisecond value and borrow no memory.
        let active = unsafe { timeBeginPeriod(1) } == 0;
        if !active {
            tracing::warn!("1 ms timer request unavailable; sender pacing may have more jitter");
        }
        ClockGuard { active }
    }
    #[cfg(not(windows))]
    {
        ClockGuard
    }
}

#[cfg(windows)]
impl Drop for ClockGuard {
    fn drop(&mut self) {
        if self.active {
            unsafe {
                timeEndPeriod(1);
            }
        }
    }
}
