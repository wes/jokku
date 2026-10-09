import { DocPage } from '../../components/doc-page';

export default async function DocsIndexPage() {
  return <DocPage slug="introduction" />;
}

export const getConfig = async () => {
  return {
    render: 'static',
  } as const;
};
