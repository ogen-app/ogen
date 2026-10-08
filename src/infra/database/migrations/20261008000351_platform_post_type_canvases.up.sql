-- CON-351: recommended artwork size per post type.
--
-- Keyed by post-type slug: {"story": {"width": 1080, "height": 1920}}. Only
-- media-bearing types (image-post, carousel, video, reel, short, story) carry
-- one; text-only types have no canvas. Each value is the platform's
-- recommended upload size, and every video type's ratio is one the
-- platform's video_constraints allow.
ALTER TABLE platforms ADD COLUMN post_type_canvases JSONB NOT NULL DEFAULT '{}';

-- Sizes checked against docs.zernio.com/platforms/{slug} and each platform's
-- own creative specs on 2026-10-08.

-- X: 16:9 landscape; video stays inside its 1920×1200 cap.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1600,"height":900},"video":{"width":1920,"height":1080}}'
    WHERE id = '81mUCmc2xsKd';

-- LinkedIn: link-style image, portrait document carousel, landscape video.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1200,"height":627},"carousel":{"width":1080,"height":1350},"video":{"width":1920,"height":1080}}'
    WHERE id = 'AXqWG7U2qnpt';

-- Facebook: square feed images (Meta's recommended ratio), square carousel
-- cards, 4:5 feed video, 9:16 Reels and Stories.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1080,"height":1080},"carousel":{"width":1080,"height":1080},"video":{"width":1080,"height":1350},"reel":{"width":1080,"height":1920},"story":{"width":1080,"height":1920}}'
    WHERE id = 'zBU1zqVICGfk';

-- Instagram: 4:5 feed and carousel, 9:16 Reels and Stories.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1080,"height":1350},"carousel":{"width":1080,"height":1350},"reel":{"width":1080,"height":1920},"story":{"width":1080,"height":1920}}'
    WHERE id = 'rzgpTkARLH0L';

-- YouTube: 16:9 video, 9:16 Shorts.
UPDATE platforms SET post_type_canvases =
    '{"video":{"width":1920,"height":1080},"short":{"width":1080,"height":1920}}'
    WHERE id = '8S8bWQTG6qD';

-- Threads: 4:5 images; video is 9:16 because Threads takes no 4:5 video.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1080,"height":1350},"carousel":{"width":1080,"height":1350},"video":{"width":1080,"height":1920}}'
    WHERE id = 'pQ4yxT3SuE57';

-- TikTok: full-screen 9:16 for video and photo posts.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1080,"height":1920},"carousel":{"width":1080,"height":1920},"video":{"width":1080,"height":1920}}'
    WHERE id = 'Tk7nQ2xLpR9a';

-- Pinterest: 2:3 Pins.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1000,"height":1500},"video":{"width":1000,"height":1500}}'
    WHERE id = 'Pn4vK8mWz1Bc';

-- Reddit: Zernio's recommended 1200×628 image, reused for gallery images;
-- 16:9 video inside Reddit's 1080p cap.
UPDATE platforms SET post_type_canvases =
    '{"image-post":{"width":1200,"height":628},"carousel":{"width":1200,"height":628},"video":{"width":1920,"height":1080}}'
    WHERE id = 'Rd5hJ3yTq6Ne';
