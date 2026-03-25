export default {
  async email(message, env, ctx) {
    const chunks = [];
    for await (const chunk of message.raw) {
      chunks.push(chunk);
    }

    let size = chunks.reduce((a, b) => a + b.length, 0);
    let raw = new Uint8Array(size);
    let offset = 0;
    for (const c of chunks) {
      raw.set(c, offset);
      offset += c.length;
    }

    const response = await fetch(env.SERVER_ENDPOINT, {
      method: "POST",
      headers: {
        "Content-Type": "application/octet-stream",
        "X-Webhook-Secret": env.WEBHOOK_SECRET,
      },
      body: raw,
    });

    if (!response.ok) {
      let body = "";
      try {
        body = await response.text();
      } catch {
        body = "";
      }

      throw new Error(`backend returned ${response.status}${body ? `: ${body}` : ""}`);
    }
  }
};
