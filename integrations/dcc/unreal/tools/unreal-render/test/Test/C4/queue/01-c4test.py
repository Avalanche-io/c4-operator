# The unreal-render tool's test job, built in the test project by the editor
# (unreal_render.py runs it with -run=pythonscript when a job's config is a
# .py): a map /Game/C4Test with a cube, a light and a cine camera, a level
# sequence /Game/C4Test_seq over 1001-1003 cut to that camera, and a queue of
# one job (64x36 PNG, {sequence_name}.{frame_number}) written as the queue
# manifest at $C4_UNREAL_MANIFEST, as the Unreal executor writes a job's.
import os

import unreal

out = os.environ["C4_UNREAL_MANIFEST"]

levels = unreal.get_editor_subsystem(unreal.LevelEditorSubsystem)
actors = unreal.get_editor_subsystem(unreal.EditorActorSubsystem)
levels.new_level("/Game/C4Test")
actors.spawn_actor_from_object(unreal.load_asset("/Engine/BasicShapes/Cube"), unreal.Vector(0, 0, 50))
actors.spawn_actor_from_class(unreal.DirectionalLight, unreal.Vector(0, 0, 400), unreal.Rotator(roll=0, pitch=-45, yaw=30))
actors.spawn_actor_from_class(unreal.SkyLight, unreal.Vector(0, 0, 400))
cam = actors.spawn_actor_from_class(unreal.CineCameraActor, unreal.Vector(-400, 0, 120), unreal.Rotator(roll=0, pitch=-10, yaw=0))
levels.save_current_level()

seq = unreal.AssetToolsHelpers.get_asset_tools().create_asset("C4Test_seq", "/Game", unreal.LevelSequence,
                                                               unreal.LevelSequenceFactoryNew())
seq.set_display_rate(unreal.FrameRate(24, 1))
seq.set_playback_start(1001)
seq.set_playback_end(1003)
binding = seq.add_possessable(cam)
cut = seq.add_track(unreal.MovieSceneCameraCutTrack).add_section()
cut.set_range(1001, 1003)
try:
    cut.set_camera_binding_id(seq.get_portable_binding_id(seq, binding))
except AttributeError:
    cut.set_camera_binding_id(seq.get_binding_id(binding))
unreal.EditorAssetLibrary.save_loaded_asset(seq)

queue = unreal.new_object(unreal.MoviePipelineQueue)
job = queue.allocate_new_job(unreal.MoviePipelineExecutorJob)
job.job_name = "c4test"
job.map = unreal.SoftObjectPath("/Game/C4Test.C4Test")
job.sequence = unreal.SoftObjectPath("/Game/C4Test_seq.C4Test_seq")
config = job.get_configuration()
config.find_or_add_setting_by_class(unreal.MoviePipelineDeferredPassBase)
config.find_or_add_setting_by_class(unreal.MoviePipelineImageSequenceOutput_PNG)
output = config.find_or_add_setting_by_class(unreal.MoviePipelineOutputSetting)
output.output_resolution = unreal.IntPoint(64, 36)
output.file_name_format = "{sequence_name}.{frame_number}"

saved = unreal.MoviePipelineEditorLibrary.save_queue_to_manifest_file(queue)
where = next(x for x in (saved if isinstance(saved, (tuple, list)) else [saved]) if isinstance(x, str))
with open(out, "w", encoding="utf-8") as f:
    f.write(unreal.MoviePipelineEditorLibrary.convert_manifest_file_to_string(where))
