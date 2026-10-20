# C4 for Nuke

The panel artists use to submit a Nuke script to a C4 farm, from inside Nuke.
It needs no terminal. Each Write node is a job. A Write that reads another
Write's render waits for it.

## Install

Add this folder to `NUKE_PATH`, and keep `../c4submit` beside it, as it is in
the repository. `menu.py` finds it there. Set these in the studio's
environment:

| variable                           | what it is                                                                                                                                                                                  |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `C4_HUD`                           | the farm's address, for example `https://c4.petrelfinch.example`                                                                                                                            |
| `C4_HUD_TOKEN_FILE`                | a file holding the artist's sign-in token, read on each request so a renewed token takes effect (or `C4_HUD_TOKEN`)                                                                         |
| `C4_PROJECT`                       | the project, for example `saltmarsh`. The artist can change it in the panel                                                                                                                 |
| `C4_ROOT`                          | the folder on this machine that is the top of the project's tree, for example `/show`. Every file a script reads must be under it                                                           |
| `C4_SHOT` or `C4_SHOT_PATTERN`     | the shot, or a regular expression whose `shot` group finds it in the script's path, for example `/(?P<shot>sm\d{4})/`                                                                        |
| `C4_TOOL_PATH`                     | optional: folders of tools (each holding a `tool.yaml`), used when the farm does not list its tools yet, so the panel can still draw the form and save the submission as a file              |

Nuke 14, 15 and 16 are supported. Nuke 13 and earlier (Python 3.7) are not.

## What the artist sees

**C4 > Submit to farm...** opens the panel. Right-clicking in the Node Graph
gives **C4 > Submit this Write to C4**, which opens it with only the selected
Write ticked. Top to bottom:

- **Tool**: the farm's tools that render Nuke scripts.
- **Project** and **Shot**.
- **Needed by**: the project's milestones (Dailies, Client review),
  **When the farm suggests**, or **At a time I type** ("fri 18:00", "+3h").
  This is the only urgency the artist sets.
- **Frames**: linked to each Write's own range (or the script's) until the
  artist types into it.
- **One checkbox per Write node**, with its range and what it comes after.
  Disabled Writes are not listed.
- **Size**, when the tool has sizes, and **Note**.
- **The tool's own parameters**, drawn from its definition. A value taken
  from the script has a **linked** box beside it. Advanced ones are folded
  away.
- **Check**, **Submit**, **Show file**, **Save file...**, and the summary.

**Check** saves the script, then reads every file the ticked Writes need,
asks the farm what it lacks, and asks the farm to check the submission. All of
this runs behind a progress bar that can be cancelled. The summary shows each
job, the files found and how many must be sent, the estimate, and the slack
against Needed by. Errors come with their fix. **Show file** shows the
submission as a file. **Save file...** saves it, to submit later with
`c4 farm submit FILE`.

If the farm lists no tools for the project, the panel says so and draws no
form. Setting `C4_TOOL_PATH` to the studio's tool folders lets it draw the form
and save the file. Check then reads the files and checks the file locally,
and the submission waits until the farm takes it.

## On Submit

Submit is available only after a check passed and nothing has changed since,
whether in the panel or in the script. It sends only the files the farm lacks
and records them as the job's tree. It then submits one job per ticked Write,
each `after` the Writes whose renders it reads. The summary lists the jobs the
farm made. Submitting the same script again sends nothing new.

## Tests

```
python -m pytest -q integrations/dcc/nuke
```

The tests use a fake `nuke` (`tests/fakenuke.py`) and c4submit's fake farm,
with real files. What only a real Nuke can show is in
[CHECKLIST.md](CHECKLIST.md).
