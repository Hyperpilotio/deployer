import unittest
import requests
import os
import json
import time


class Deployer:
    DEPLOYER_BASE_URL = "http://internal-deployer-605796188.us-east-1.elb.amazonaws.com:7777"
    DEPLOYER_REST_URL = '/'.join([DEPLOYER_BASE_URL, 'v1', 'deployments'])
    DEPLOYMENT_NAME = ""
    DEPLOYMENT_CREATE_ERROR = False
    TIMEOUT = 60 * 30

    def running(self):
        r = requests.get('/'.join([deployer.DEPLOYER_BASE_URL, 'ui']))
        return r.status_code

    def create_k8s_deployement(self, datas):
        headers = {'Content-type': 'application/json'}
        resp = requests.post(deployer.DEPLOYER_REST_URL,
                             json=datas, headers=headers)
        resp_data = json.loads(resp.content)
        deployer.DEPLOYMENT_NAME = resp_data['data'].split()[
            2].replace('.', '')
        deployer.DEPLOYMENT_CREATE_ERROR = resp_data['error']
        return deployer.DEPLOYMENT_CREATE_ERROR

    def wait_until_deployement_state_available(self, deployment_name):
        mustend = time.time() + deployer.TIMEOUT
        deployement_state = ""
        while time.time() < mustend:
            deployement_state = self.get_deployement_state(deployment_name)
            if deployement_state == "Available":
                return deployement_state
            time.sleep(10)
        return deployement_state

    def get_deployement_state(self, deployment_name):
        resp = requests.get(
            '/'.join([deployer.DEPLOYER_REST_URL, deployment_name, 'state']))
        return resp.content

    def get_service_url(self, deployment_name, service_name):
        resp = requests.get(
            '/'.join([deployer.DEPLOYER_REST_URL, deployment_name,
                      'services', service_name, 'url']))
        return resp


class DeployerTest(unittest.TestCase):
    def test_01_is_running(self):
        self.assertEqual(deployer.running(), 200)

    def test_02_create_k8s(self):
        with open('deployer_test.json') as data_file:
            json_data = json.load(data_file)
        self.assertEqual(deployer.create_k8s_deployement(json_data), False)

    def test_03_get_deployment_state(self):
        if deployer.DEPLOYMENT_CREATE_ERROR == False:
            self.assertEqual(deployer.wait_until_deployement_state_available(
                deployer.DEPLOYMENT_NAME), "Available")

    def test_04_get_service_url(self):
        resp = deployer.get_service_url(
            deployer.DEPLOYMENT_NAME, "nginx")
        self.assertEqual(resp.status_code, 200)


if __name__ == '__main__':
    deployer = Deployer()
    unittest.main()
