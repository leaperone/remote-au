package transport

import "testing"

func TestFrameTrackerAcceptTracksGapsAndStaleFrames(t *testing.T) {
	var tracker frameTracker

	first := tracker.Accept(10, 100, 480)
	if first.Stale || first.MissingPackets != 0 || first.CaptureGapFrames != 0 || first.GapFrames != 0 {
		t.Fatalf("first accept result = %#v, want no gaps", first)
	}
	if tracker.expectedSeq != 11 || tracker.expectedCaptureFrame != 580 {
		t.Fatalf("tracker after first frame = seq %d capture %d, want seq 11 capture 580", tracker.expectedSeq, tracker.expectedCaptureFrame)
	}

	gap := tracker.Accept(12, 1540, 480)
	if gap.Stale {
		t.Fatalf("gap frame marked stale")
	}
	if gap.ExpectedSeq != 11 || gap.MissingPackets != 1 {
		t.Fatalf("sequence gap = expected %d missing %d, want expected 11 missing 1", gap.ExpectedSeq, gap.MissingPackets)
	}
	if gap.ExpectedCaptureFrame != 580 || gap.CaptureGapFrames != 960 || gap.GapFrames != 960 {
		t.Fatalf("capture gap = %#v, want expected 580 raw/capped 960", gap)
	}
	if tracker.expectedSeq != 13 || tracker.expectedCaptureFrame != 2020 {
		t.Fatalf("tracker after gap frame = seq %d capture %d, want seq 13 capture 2020", tracker.expectedSeq, tracker.expectedCaptureFrame)
	}

	stale := tracker.Accept(11, 580, 480)
	if !stale.Stale {
		t.Fatalf("late frame was not marked stale: %#v", stale)
	}
	if stale.ExpectedSeq != 13 {
		t.Fatalf("stale expected seq = %d, want 13", stale.ExpectedSeq)
	}
	if tracker.expectedSeq != 13 || tracker.expectedCaptureFrame != 2020 {
		t.Fatalf("stale frame advanced tracker to seq %d capture %d", tracker.expectedSeq, tracker.expectedCaptureFrame)
	}
}

func TestFrameTrackerCapsCaptureGapFrames(t *testing.T) {
	tracker := frameTracker{maxGapFrames: 100}

	tracker.Accept(1, 0, 10)
	gap := tracker.Accept(2, 1000, 10)
	if gap.CaptureGapFrames != 990 {
		t.Fatalf("raw capture gap = %d, want 990", gap.CaptureGapFrames)
	}
	if gap.GapFrames != 100 {
		t.Fatalf("capped gap = %d, want 100", gap.GapFrames)
	}
}

func TestFrameTrackerFallsBackToMissingPacketFrames(t *testing.T) {
	tracker := frameTracker{maxGapFrames: 100}

	tracker.Accept(1, 0, 10)
	gap := tracker.Accept(3, 10, 10)
	if gap.MissingPackets != 1 {
		t.Fatalf("missing packets = %d, want 1", gap.MissingPackets)
	}
	if gap.CaptureGapFrames != 0 {
		t.Fatalf("capture gap = %d, want 0", gap.CaptureGapFrames)
	}
	if gap.GapFrames != 10 {
		t.Fatalf("fallback gap = %d, want 10", gap.GapFrames)
	}
}
