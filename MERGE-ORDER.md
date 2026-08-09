# Do not merge this branch alone

The README change on this branch documents two provider keys,
`structured_output` and `max_output_tokens`. **The code that reads them is not
on this branch.** It is the four-commit stack on
`feat/claude-structured-output`, which is unmerged:

```console
$ git merge-base --is-ancestor 56a9c49 origin/master; echo $?
1
$ git branch -a --contains 56a9c49
+ feat/claude-structured-output
  remotes/origin/feat/claude-structured-output
```

## Why this is worse than a broken build

`config.Load` decodes with `yaml.Unmarshal` and no `KnownFields(true)`
(`internal/config/config.go`), so an unknown provider key is not an error — it
is discarded without a word. The only strict decoder in the tree is the one for
batch task files (`internal/engine/batch.go`).

An operator who follows the new *Models and providers* example against a build
without that stack gets no error, no schema opt-out and no raised ceiling. The
keys are dropped in silence, which is the one failure mode the config loader
cannot warn them about.

## Either resolution is fine

1. Land `feat/claude-structured-output` first, then this. Or,
2. merge the two together.

The merge is conflict-free by construction: that branch touches only Go files,
this one only `README.md`, and both sit directly on `08a7113`. It was attempted
here and failed for an environmental reason only — the repository's `.git` is
mounted read-only in the agent sandbox (`error: unable to create temporary
file: Read-only file system`), so the merge commit has to be made by whoever
integrates this.

## Check before merging, and then delete this file

```console
$ git merge-base --is-ancestor 56a9c49 HEAD && echo "safe to merge"
```

Once that prints, this file has done its job — **delete it in the merge**. It
documents a temporary constraint, not the project.
