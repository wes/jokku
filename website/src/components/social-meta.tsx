import { absoluteUrl, OG_IMAGE } from '../lib/site';

// A page's title, description and link preview (Open Graph and Twitter).
// Every page renders one; the root layout adds nothing of its own, so no tag
// appears twice.
export const SocialMeta = ({
  title,
  description,
  path,
}: {
  title: string;
  description: string;
  path: string;
}) => {
  const url = absoluteUrl(path);
  const image = absoluteUrl(OG_IMAGE.path);
  return (
    <>
      <title>{title}</title>
      <meta name="description" content={description} />
      {url.startsWith('http') ? <link rel="canonical" href={url} /> : null}

      <meta property="og:type" content="website" />
      <meta property="og:site_name" content="Jokku" />
      <meta property="og:title" content={title} />
      <meta property="og:description" content={description} />
      <meta property="og:url" content={url} />
      <meta property="og:image" content={image} />
      <meta property="og:image:type" content={OG_IMAGE.type} />
      <meta property="og:image:width" content={String(OG_IMAGE.width)} />
      <meta property="og:image:height" content={String(OG_IMAGE.height)} />
      <meta property="og:image:alt" content={OG_IMAGE.alt} />

      <meta name="twitter:card" content="summary_large_image" />
      <meta name="twitter:title" content={title} />
      <meta name="twitter:description" content={description} />
      <meta name="twitter:image" content={image} />
      <meta name="twitter:image:alt" content={OG_IMAGE.alt} />
    </>
  );
};
