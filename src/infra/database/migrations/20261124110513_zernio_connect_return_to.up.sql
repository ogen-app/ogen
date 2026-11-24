-- Where the OAuth callback sends the browser back to: '' is Settings,
-- 'onboarding' is setup after signup.
ALTER TABLE zernio_connect_sessions
    ADD COLUMN return_to TEXT NOT NULL DEFAULT '' CHECK (return_to IN ('', 'onboarding'));
