-- Whether an address from the provider joins someone to the account that has it, when the provider does not say
-- that the address is verified. Some providers never say.
ALTER TABLE sso_config ADD COLUMN trust_emails INTEGER NOT NULL DEFAULT 0;
