import type { PageProps } from 'waku/router';
import { DocPage } from '../../components/doc-page';
import { allSlugs } from '../../lib/docs';

export default async function DocsSlugPage({ slug }: PageProps<'/docs/[slug]'>) {
  return <DocPage slug={slug} />;
}

export const getConfig = async () => {
  return {
    render: 'static',
    staticPaths: allSlugs().filter((slug) => slug !== 'introduction'),
  } as const;
};
