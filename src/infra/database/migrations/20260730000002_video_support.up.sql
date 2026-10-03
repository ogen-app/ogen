-- CON-148: video pipeline. Add per-platform video rule set + video
-- metadata on attachments.
--
-- video_constraints mirrors image_constraints / pdf_constraints: a jsonb
-- rule set on the platform row. A zero value ('{}') means "this platform
-- does not accept video", which the validator surfaces as a soft warning.
-- duration_ms / codec carry the video-service probe result on the
-- attachment (width/height are reused for the frame size).

ALTER TABLE platforms
    ADD COLUMN video_constraints jsonb NOT NULL DEFAULT '{}';

ALTER TABLE post_attachments
    ADD COLUMN duration_ms BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN codec       TEXT   NOT NULL DEFAULT '';

-- Backfill the six video-capable platforms. Values are platform-level
-- ceilings (bytes / seconds / pixels); per-post-type floors (e.g. Reels
-- length) stay in the post-type rules. Seeded by id so the backfill is
-- stable regardless of name changes.

-- Facebook: feed video + Reels; mp4/mov; up to ~4 GB / 240 min.
UPDATE platforms SET video_constraints =
    '{"max_file_size_bytes":4294967296,"allowed_formats":["mp4","mov"],"max_duration_seconds":14400,"min_duration_seconds":0,"max_width":1920,"max_height":1920,"allowed_aspect_ratios":["16:9","9:16","1:1","4:5"],"max_attachments_per_post":1}'
    WHERE id = 'zBU1zqVICGfk';

-- Instagram: Reels + feed video; mp4/mov; up to ~1 GB / 15 min; 3s floor.
UPDATE platforms SET video_constraints =
    '{"max_file_size_bytes":1073741824,"allowed_formats":["mp4","mov"],"max_duration_seconds":900,"min_duration_seconds":3,"max_width":1920,"max_height":1920,"allowed_aspect_ratios":["9:16","1:1","4:5","16:9"],"max_attachments_per_post":1}'
    WHERE id = 'rzgpTkARLH0L';

-- LinkedIn: native video; mp4; up to ~5 GB / 15 min; 3s floor.
UPDATE platforms SET video_constraints =
    '{"max_file_size_bytes":5368709120,"allowed_formats":["mp4"],"max_duration_seconds":900,"min_duration_seconds":3,"max_width":4096,"max_height":2304,"allowed_aspect_ratios":["16:9","1:1","9:16"],"max_attachments_per_post":1}'
    WHERE id = 'AXqWG7U2qnpt';

-- Threads: video; mp4/mov; up to ~1 GB / 5 min.
UPDATE platforms SET video_constraints =
    '{"max_file_size_bytes":1073741824,"allowed_formats":["mp4","mov"],"max_duration_seconds":300,"min_duration_seconds":0,"max_width":1920,"max_height":1920,"allowed_aspect_ratios":["16:9","9:16","1:1"],"max_attachments_per_post":1}'
    WHERE id = 'pQ4yxT3SuE57';

-- X (Twitter): video; mp4/mov; up to ~512 MB / 140 s (standard tier).
UPDATE platforms SET video_constraints =
    '{"max_file_size_bytes":536870912,"allowed_formats":["mp4","mov"],"max_duration_seconds":140,"min_duration_seconds":0,"max_width":1920,"max_height":1200,"allowed_aspect_ratios":["16:9","1:1"],"max_attachments_per_post":1}'
    WHERE id = '81mUCmc2xsKd';

-- YouTube: long-form video + Shorts; mp4/mov/webm/avi/mkv; up to ~64 GB / 12 h.
-- requires_video_title: a product rule, not a platform one. Zernio accepts an
-- untitled upload but titles it from the first line of the description, else
-- "Untitled Video", so publishing is blocked until the post has a title.
UPDATE platforms SET video_constraints =
    '{"max_file_size_bytes":68719476736,"allowed_formats":["mp4","mov","webm","avi","mkv"],"max_duration_seconds":43200,"min_duration_seconds":0,"max_width":7680,"max_height":4320,"allowed_aspect_ratios":["16:9","9:16"],"max_attachments_per_post":1,"requires_video_title":true}'
    WHERE id = '8S8bWQTG6qD';
