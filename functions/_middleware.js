export async function onRequest(context) {
  const { request } = context;
  const url = new URL(request.url);

  // If the request hostname starts with "www.", redirect to the non-www version
  if (url.hostname.startsWith("www.")) {
    url.hostname = url.hostname.replace(/^www\./, "");
    return Response.redirect(url.toString(), 301);
  }

  // Otherwise, continue to serve the static assets or next handlers
  return await context.next();
}
