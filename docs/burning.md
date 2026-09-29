# Burning and checking discs

Open **Burn ISO** and choose **Burn ISO files** to burn a new batch or resume
saved progress. Choose **Verify disc against ISO** to check an existing disc:
select its original ISO, insert the recorded disc, and choose the drive holding
it. Verification reads and compares every byte of the ISO without writing or
ejecting the disc. Extra disc padding after the ISO is ignored. A short read,
unreadable disc, or byte mismatch is reported as a failed check. Press Esc to
stop a check, or R after it finishes to check the same ISO and drive again.

When a burn or its automatic verification fails, press Enter to retry with a
blank disc, or **S** to skip the failed ISO and any copies still in the queue.
Copies already assigned to other drives continue. Use Tab or the up/down arrow
keys to choose between drives waiting for discs. S also works from the progress
screen after dismissing a failed drive's dialog with Esc.

Skipped discs are saved separately from successfully burned discs and stay
skipped when an interrupted batch is resumed. The completed batch shows how
many discs were burned and how many were skipped. To burn a skipped ISO later,
select it in a new batch.
