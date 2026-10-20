# Sample sets

`saltmarsh.py` writes the text and image parts of the `c4 farm samples` set
(stdlib Python, plus `oiiotool` for the plates); `build.sh VERSION` adds the
parts made in the job images (`fog.blend`, the LoRA style frames) and packs
`c4-samples-VERSION.tar.gz` with its C4 ID beside it, the two files a
release attaches. `build.sh` needs Docker and the job images; it is run and
checked in a Docker run.
