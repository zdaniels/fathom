export async function search(params: {
  query: string;
  maxResults?: number;
}): Promise<{ results: Array<{ title: string; url: string; snippet: string }>; query: string }> {
  const { query, maxResults = 5 } = params;
  const url = `https://api.duckduckgo.com/?q=${encodeURIComponent(query)}&format=json&no_html=1`;
  const res = await fetch(url);
  const data = (await res.json()) as {
    RelatedTopics?: Array<{ Text?: string; FirstURL?: string }>;
    AbstractText?: string;
    AbstractURL?: string;
  };

  const results: Array<{ title: string; url: string; snippet: string }> = [];

  if (data.AbstractText && data.AbstractURL) {
    results.push({
      title: query,
      url: data.AbstractURL,
      snippet: data.AbstractText,
    });
  }

  const topics = data.RelatedTopics ?? [];
  for (const topic of topics) {
    if (topic.Text && topic.FirstURL) {
      results.push({
        title: topic.Text.slice(0, 100),
        url: topic.FirstURL,
        snippet: topic.Text,
      });
      if (results.length >= maxResults) break;
    }
  }

  return {
    results: results.slice(0, maxResults),
    query,
  };
}

export const tools = [
  {
    name: "web_search",
    description: "Search the web for information",
    parameters: {
      type: "object",
      properties: {
        query: { type: "string", description: "Search query" },
        maxResults: { type: "number", description: "Maximum number of results", default: 5 },
      },
      required: ["query"],
    },
  },
];
