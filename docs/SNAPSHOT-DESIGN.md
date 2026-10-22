# Git Snapshot Design for Change Artifacts

Status: planned. This document describes how git workdirs will be captured for
change artifacts. Non-git workdirs keep the existing opt-in hash/glob capture.

## 1. Problems

The current git baseline stores only `HEAD` and computes changes with
`git diff <HEAD>` plus `git status`. Two problems follow:

1. **Pre-existing changes are attributed to the task.** Uncommitted and
   untracked files that existed before the task starts appear in the task's
   diff. This is especially visible in a shared workdir.
2. **Untracked content is missing.** Untracked files appear only as names in
   the diff; their content is emitted only if a per-file or image artifact is
   generated.

## 2. Goals

- Capture the exact workdir delta produced during one task.
- Include pre-existing dirty and untracked files in the baseline so they cancel
  out.
- Include untracked file content in the diff, bounded by the diff limit.
- Avoid hashing every file in a git workdir.
- Keep non-git capture unchanged: opt-in globs plus content hashing.

Non-goals: restore/rollback, incremental streaming, snapshotting non-git
projects.

## 3. Approach

Use a temporary shadow Git repository per task:

- Real workdir: the task's resolved directory.
- Shadow git directory: a temporary directory owned by the task.
- Shadow index: a copy of the real repository's index, so unchanged tracked
  files reuse Git's stat cache.
- Object alternates: point the shadow repository at the real repository's
  objects, so existing blobs are not copied.

At task start, capture a **baseline tree** of the current worktree. At task end,
capture a **current tree**. The task delta is the tree diff between them.

## 4. Shadow Repository Setup

For each task with a git workdir:

1. Resolve:
   - worktree root
   - real git directory (`git rev-parse --absolute-git-dir`)
   - common objects directory (`git rev-parse --git-path objects`)
   - real index path (`git rev-parse --git-path index`)
2. Create a temporary git directory and run `git init` with the worktree.
3. Configure the shadow repository:
   - `core.autocrlf=false`
   - `core.longpaths=true`
   - `core.symlinks=true`
   - `feature.manyFiles=true`
   - `index.version=4`
   - `index.threads=true`
   - `core.untrackedCache=true`
4. Write `<shadow>/objects/info/alternates` pointing at the real objects
   directory.
5. Copy the real index into `<shadow>/index` when it exists.
6. Add `.a2a/` and `.harbor/` to `<shadow>/info/exclude`.

Commands use:

```text
git --git-dir=<shadow> --work-tree=<worktree> ...
```

The shadow repository and index are deleted when the task finishes, fails, or
is canceled. Objects therefore do not accumulate in the project repository.

## 5. Capture Algorithm

Capture runs at baseline and at the end of the task:

1. Collect candidate paths:
   - `git diff-files --name-only -z -- .`
   - `git ls-files --others --exclude-standard -z -- .`
2. Drop ignored and oversized candidates.
3. Stage the remaining candidates into the shadow index:
   - `git add --all --sparse --pathspec-from-file=- --pathspec-file-nul`
4. Remove ignored or oversized entries already present in the shadow index:
   - `git rm --cached -f --ignore-unmatch --pathspec-from-file=- --pathspec-file-nul`
5. Write the tree:
   - `git write-tree`

The baseline tree contains pre-existing tracked changes and untracked files.
The current tree contains the same worktree after the task. Their diff is the
task delta, so earlier changes do not leak into the artifact.

## 6. Diff and Files

Use tree-to-tree comparisons:

- Files: `git diff --name-status -z <baseline> <current>`
- Diff: `git diff <baseline> <current>`

Requirements:

- Preserve rename targets for file artifacts; use `--name-status -z` parsing
  with rename/copy awareness.
- Emit unified diff text and mark the result as unified.
- Keep `.a2a/` and `.harbor/` excluded.
- Binary files report "Binary files ... differ" without content.
- Apply `sandbox.diff_inline_max_bytes` to the final diff.

## 7. Untracked Size Limit

Add a configurable cap:

```yaml
sandbox:
  snapshot_untracked_max_bytes: 2097152
```

Semantics:

- Untracked files at or below the cap are staged into the tree and their
  content appears in the diff.
- Larger untracked files are not staged. Their paths must still be recorded in
  the baseline and reported as changed/omitted; content is not included.
- The cap applies per untracked file. It is independent of
  `artifact_inline_max_bytes`, which bounds per-file artifact content.

If path-only reporting for oversized untracked files is not implemented, the
limitation must be explicit in user documentation.

## 8. Failure and Cleanup

- Snapshot setup or capture failure marks change capture unavailable for that
  task.
- Do not fall back to `git diff HEAD`; that reintroduces pre-existing-change
  attribution.
- Remove the shadow repository and index on every terminal path, including
  cancellation.
- If cleanup fails, log the path for operator reclamation.

## 9. Efficiency

- Unchanged tracked files are not re-read: the copied index provides stat
  metadata.
- The shadow object store reuses real objects through alternates; only changed
  and untracked content is written as new blobs.
- The remaining cost is directory traversal and `lstat` from
  `diff-files`/`ls-files` at baseline and capture, plus writing changed content.
- Very large worktrees can still be slow on cold cache or network filesystems.
  `feature.manyFiles`, index v4, and the untracked cache reduce this cost.
- Shared workdirs with a dirty baseline pay for capturing that dirty state in
  the baseline tree; that cost is required for correct attribution.

## 10. Non-Git Workdirs

Non-git workdirs do not use trees. They keep the existing opt-in behavior:

- No `sandbox.non_git_change_globs` means no change capture.
- With globs, only matching files are hashed.
- The result remains an added/modified/removed summary.

Non-git summaries use `text/plain`; git unified diffs use `text/x-diff`. The diff
artifact metadata records the format as `summary` or `unified`.

## 11. Tests

- Pre-existing modified file does not appear in the task diff.
- Pre-existing untracked file does not appear in the task diff.
- Newly created untracked file appears with content.
- Rename produces the new path and content.
- Deleted file appears in the diff and does not produce a file artifact.
- `.a2a/` and `.harbor/` changes are excluded.
- Oversized untracked file is reported without content.
- Snapshot setup failure disables changes without falling back to `HEAD`.
- Shadow repository is removed on success, failure, and cancellation.
- Non-git summary uses the correct MIME type.
