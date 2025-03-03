import { DELETE } from '@/services/request';
import { BaseResourceType, ResourceKindType } from '@/types/resource';

export const delCronJobByName = (instanceName: string) =>
  DELETE('/api/resource/deleteCronjob', { instanceName });

export const deleteAppCRD = (instanceName: string) =>
  DELETE('/api/resource/deleteAppCRD', { instanceName });

export const deleteSecret = (instanceName: string) =>
  DELETE('/api/resource/deleteSecret', { instanceName });

export const delApplaunchpad = (instanceName: string) =>
  DELETE('/api/resource/delApplaunchpad', { instanceName });

export const delDBByName = (instanceName: string) =>
  DELETE('/api/resource/delDBByName', { instanceName });

export const delInstanceByName = (instanceName: string) =>
  DELETE('/api/instance/deleteByName', { instanceName });

export const delJobByName = (instanceName: string) =>
  DELETE('/api/resource/delJob', { instanceName });

export const delConfigMapByName = (instanceName: string) =>
  DELETE('/api/resource/delConfigMap', { instanceName });

export const delIssuerByName = (instanceName: string) =>
  DELETE('/api/resource/deleteIssuer', { instanceName });

export const delRoleByName = (instanceName: string) =>
  DELETE('/api/resource/delRole', { instanceName });

export const delRoleBindingByName = (instanceName: string) =>
  DELETE('/api/resource/delRoleBinding', { instanceName });

export const delServiceAccountByName = (instanceName: string) =>
  DELETE('/api/resource/delServiceAccount', { instanceName });

const deleteResourceByKind: Record<ResourceKindType, (instanceName: string) => void> = {
  CronJob: (instanceName: string) => delCronJobByName(instanceName),
  App: (instanceName: string) => deleteAppCRD(instanceName),
  Secret: (instanceName: string) => deleteSecret(instanceName),
  AppLaunchpad: (instanceName: string) => delApplaunchpad(instanceName),
  DataBase: (instanceName: string) => delDBByName(instanceName),
  Instance: (instanceName: string) => delInstanceByName(instanceName),
  Job: (instanceName: string) => delJobByName(instanceName),
  ConfigMap: (instanceName: string) => delConfigMapByName(instanceName),
  Issuer: (instanceName: string) => delIssuerByName(instanceName),
  Role: (instanceName: string) => delRoleByName(instanceName),
  RoleBinding: (instanceName: string) => delRoleBindingByName(instanceName),
  ServiceAccount: (instanceName: string) => delServiceAccountByName(instanceName)
};

export const deleteAllResources = async (resources: BaseResourceType[]) => {
  const deletePromises = resources.map((resource) => {
    return deleteResourceByKind[resource.kind](resource.name);
  });
  const reuslt = await Promise.allSettled(deletePromises);
  console.log(reuslt);
  return reuslt;
};
