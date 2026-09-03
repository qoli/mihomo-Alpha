# Fork intent maintenance

Before syncing upstream or changing this fork's Smart behavior or binary distribution,
read [docs/fork-intent/README.md](docs/fork-intent/README.md) and the relevant linked intent document.

- The direct comparison upstream is `vernesong/mihomo`, branch `Alpha`. Verify the URL;
  a local remote named `upstream` may point to `MetaCubeX/mihomo` instead.
- Review changes against the SC-01 through SC-05 and CI-01 behavioral contracts,
  including upstream cache, tracker, relay and workflow changes outside the current patch.
- A conflict-free merge is not evidence that these contracts still hold. Record whether
  each affected intent is retained, replaced by an equivalent upstream implementation,
  adapted, or explicitly retired, with verification evidence.
- Update the intent index and relevant document in the same change when behavior,
  assumptions, verification status or the accepted upstream baseline changes. Preserve
  known limitations; do not rewrite intent merely to normalize an unexplained regression.
- These documents do not independently authorize upstream merges, pushes or releases.
