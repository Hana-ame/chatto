import { Code, ConnectError } from '@connectrpc/connect';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { removeRegisteredServerQueries } from './cacheRegistry';
import { queryClient } from './client';

describe('server query cache', () => {
  afterEach(() => queryClient.clear());

  it('removes only the selected server cache', () => {
    queryClient.setQueryData(['server', 'one', 'resource'], 'private-one');
    queryClient.setQueryData(['server', 'two', 'resource'], 'private-two');

    removeRegisteredServerQueries('one');

    expect(queryClient.getQueryData(['server', 'one', 'resource'])).toBeUndefined();
    expect(queryClient.getQueryData(['server', 'two', 'resource'])).toBe('private-two');
  });

  it('does not retry authentication or permission failures', async () => {
    const queryFn = vi.fn().mockRejectedValue(new ConnectError('denied', Code.PermissionDenied));

    await expect(
      queryClient.fetchQuery({ queryKey: ['server', 'one', 'denied'], queryFn })
    ).rejects.toMatchObject({ code: Code.PermissionDenied });
    expect(queryFn).toHaveBeenCalledOnce();
  });

  it('retries one transient failure', async () => {
    const queryFn = vi.fn().mockRejectedValueOnce(new Error('offline')).mockResolvedValue('ok');

    await expect(
      queryClient.fetchQuery({ queryKey: ['server', 'one', 'transient'], queryFn })
    ).resolves.toBe('ok');
    expect(queryFn).toHaveBeenCalledTimes(2);
  });
});
