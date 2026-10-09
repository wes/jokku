import { buildSearchIndex } from '../../lib/docs';

export const GET = async () =>
  new Response(JSON.stringify(buildSearchIndex()), {
    headers: { 'Content-Type': 'application/json' },
  });

export const getConfig = async () => {
  return {
    render: 'static',
  } as const;
};
