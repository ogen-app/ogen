package packs

import "github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"

// Short names for the campaign assistant's types in tests.
type (
	campaignRequest  = campaign_assistant.CampaignAssistantRequest
	campaignEvents   = campaign_assistant.OnEventFunc
	campaignResponse = campaign_assistant.CampaignAssistantResponse
)
